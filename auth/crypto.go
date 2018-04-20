// Copyright 2017 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/context/ctxhttp"

	"golang.org/x/net/context"
)

// publicKey represents a parsed RSA public key along with its unique key ID.
type publicKey struct {
	Kid string
	Key *rsa.PublicKey
}

// clock is used to query the current local time.
type clock interface {
	Now() time.Time
}

type systemClock struct{}

func (s systemClock) Now() time.Time {
	return time.Now()
}

type mockClock struct {
	now time.Time
}

func (m *mockClock) Now() time.Time {
	return m.now
}

// keySource is used to obtain a set of public keys, which can be used to verify cryptographic
// signatures.
type keySource interface {
	Keys(context.Context, *http.Client) ([]*publicKey, error)
}

// httpKeySource fetches RSA public keys from a remote HTTP server, and caches them in
// memory. It also handles cache! invalidation and refresh based on the standard HTTP
// cache-control headers.
type httpKeySource struct {
	KeyURI     string
	HTTPClient *http.Client
	CachedKeys []*publicKey
	ExpiryTime time.Time
	Clock      clock
	Mutex      *sync.Mutex
}

func newHTTPKeySource(uri string, hc *http.Client) *httpKeySource {
	return &httpKeySource{
		KeyURI:     uri,
		HTTPClient: hc,
		Clock:      systemClock{},
		Mutex:      &sync.Mutex{},
	}
}

// Keys returns the RSA Public Keys hosted at this key source's URI. Refreshes the data if
// the cache is stale.
func (k *httpKeySource) Keys(ctx context.Context, httpClient *http.Client) ([]*publicKey, error) {
	k.Mutex.Lock()
	defer k.Mutex.Unlock()
	if len(k.CachedKeys) == 0 || k.hasExpired() {
		err := k.refreshKeys(ctx, httpClient)
		if err != nil && len(k.CachedKeys) == 0 {
			return nil, err
		}
	}
	return k.CachedKeys, nil
}

// hasExpired indicates whether the cache has expired.
func (k *httpKeySource) hasExpired() bool {
	return k.Clock.Now().After(k.ExpiryTime)
}

func (k *httpKeySource) refreshKeys(ctx context.Context,
	httpClient *http.Client) error {

	k.CachedKeys = nil
	req, err := http.NewRequest("GET", k.KeyURI, nil)
	if err != nil {
		return err
	}

	client := k.HTTPClient
	if httpClient != nil {
		client = httpClient
	}

	resp, err := ctxhttp.Do(ctx, client, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	contents, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	newKeys, err := parsePublicKeys(contents)
	if err != nil {
		return err
	}
	maxAge, err := findMaxAge(resp)
	if err != nil {
		return err
	}
	k.CachedKeys = append([]*publicKey(nil), newKeys...)
	k.ExpiryTime = k.Clock.Now().Add(*maxAge)
	return nil
}

func findMaxAge(resp *http.Response) (*time.Duration, error) {
	cc := resp.Header.Get("cache-control")
	for _, value := range strings.Split(cc, ",") {
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "max-age=") {
			sep := strings.Index(value, "=")
			seconds, err := strconv.ParseInt(value[sep+1:], 10, 64)
			if err != nil {
				return nil, err
			}
			duration := time.Duration(seconds) * time.Second
			return &duration, nil
		}
	}
	return nil, errors.New("Could not find expiry time from HTTP headers")
}

func parsePublicKeys(keys []byte) ([]*publicKey, error) {
	m := make(map[string]string)
	err := json.Unmarshal(keys, &m)
	if err != nil {
		return nil, err
	}

	var result []*publicKey
	for kid, key := range m {
		pubKey, err := parsePublicKey(kid, []byte(key))
		if err != nil {
			return nil, err
		}
		result = append(result, pubKey)
	}
	return result, nil
}

func parsePublicKey(kid string, key []byte) (*publicKey, error) {
	block, _ := pem.Decode(key)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	pk, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("Certificate is not a RSA key")
	}
	return &publicKey{kid, pk}, nil
}

func parsePrivateKey(key string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(key))
	if block == nil {
		return nil, fmt.Errorf("no private key data found in: %v", key)
	}
	k := block.Bytes
	parsedKey, err := x509.ParsePKCS8PrivateKey(k)
	if err != nil {
		parsedKey, err = x509.ParsePKCS1PrivateKey(k)
		if err != nil {
			return nil, fmt.Errorf("private key should be a PEM or plain PKSC1 or PKCS8; parse error: %v", err)
		}
	}
	parsed, ok := parsedKey.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not an RSA key")
	}
	return parsed, nil
}

func verifySignature(parts []string, k *publicKey) error {
	content := parts[0] + "." + parts[1]
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}

	h := sha256.New()
	h.Write([]byte(content))
	return rsa.VerifyPKCS1v15(k.Key, crypto.SHA256, h.Sum(nil), []byte(signature))
}

type serviceAcctSigner struct {
	email string
	pk    *rsa.PrivateKey
}

func (s serviceAcctSigner) Email(ctx context.Context) (string, error) {
	if s.email == "" {
		return "", errors.New("service account email not available")
	}
	return s.email, nil
}

func (s serviceAcctSigner) Sign(ctx context.Context, ss []byte) ([]byte, error) {
	if s.pk == nil {
		return nil, errors.New("private key not available")
	}
	hash := sha256.New()
	hash.Write([]byte(ss))
	return rsa.SignPKCS1v15(rand.Reader, s.pk, crypto.SHA256, hash.Sum(nil))
}
