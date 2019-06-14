/*
Copyright 2015 All rights reserved.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
)

const (
	caCert       = "fixtures/certs/ca.crt"
	upstreamCert = "fixtures/certs/upstream.crt"
	upstreamKey  = "fixtures/certs/upstream.pem"
	appCert      = "fixtures/certs/app.crt"
	appKey       = "fixtures/certs/app.pem"
	gkCert       = "fixtures/certs/gatekeeper.crt"
	gkKey        = "fixtures/certs/gatekeeper.pem"
	authCert     = "fixtures/certs/auth.crt"
	authKey      = "fixtures/certs/auth.pem"

	e2eTLSUpstreamProxyListener = "gatekeeper.localtest.me:23328"

	e2eTLSUpstreamOauthListener    = "auth.localtest.me:13455"
	e2eTLSUpstreamUpstreamListener = "upstream.localtest.me:18511"
	e2eTLSUpstreamAppListener      = "app.localtest.me:3995"

	e2eTLSUpstreamAppURL      = "/ok"
	e2eTLSUpstreamUpstreamURL = "/apitls"

	// testPush configure test upstream server to send HTTP/2 pushed responses
	// TODO: implement suitable go client for this
	testPush = false
)

func runTestTLSUpstream(t *testing.T, listener, route string, markers ...string) error {
	go func() {
		upstreamHandler := func(w http.ResponseWriter, req *http.Request) {

			dump, _ := httputil.DumpRequest(req, false)
			nowPushing := false
			inBody := make([]string, 0, len(markers))
			inPushed := make([]string, 0, len(markers))
			t.Logf("upstream received: %q", string(dump))
			for _, m := range markers {
				if m == "push" {
					nowPushing = true
				}
				if nowPushing {
					inPushed = append(inPushed, m)
				} else {
					inBody = append(inBody, m)
				}
			}

			if pusher, ok := w.(http.Pusher); testPush && ok {
				for _, m := range inPushed {
					if err := pusher.Push("/"+m, nil); err != nil {
						// most likely, client did not enable push
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusInternalServerError)
						_, _ = io.WriteString(w, `{"error": "cannot push: `+err.Error()+`"}`)
						return
					}
				}
			} else if testPush && nowPushing {
				// for some reason push is not enabled (e.g. not http/2): should check that client & reverse proxy
				// properly enable http/2
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error": "wanted to push, but not supported"}`)
				return
			}

			_, _ = io.WriteString(w, `{"listener": "`+listener+`", "route": "`+route+`", "message": "test"`)
			for _, m := range inBody {
				_, _ = io.WriteString(w, `,"marker": "`+m+`"`)
			}
			_, _ = io.WriteString(w, `}`)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Upstream-Response-Header", "test")
		}
		http.HandleFunc(route, upstreamHandler)
		for _, m := range markers {
			http.HandleFunc("/"+m, func(w http.ResponseWriter, req *http.Request) {
				_, _ = io.WriteString(w, `{"pushed_marker": "`+m+`"}`)
				w.Header().Set("Content-Type", "application/json")
			})
		}
		_ = http.ListenAndServeTLS(listener, upstreamCert, upstreamKey, nil)
	}()
	if !assert.True(t, checkListenOrBail("https://"+path.Join(listener, route))) {
		err := fmt.Errorf("cannot connect to test https listener on: %s", "https://"+path.Join(listener, route))
		t.Logf("%v", err)
		t.FailNow()
		return err
	}
	t.Logf("test TLS upstream server: %s%s", listener, route)
	return nil
}

func runTestTLSApp(t *testing.T, listener, route string) error {
	go func() {
		mux := http.NewServeMux()
		appHandler := func(w http.ResponseWriter, req *http.Request) {
			_, _ = io.WriteString(w, `{"message": "ok"}`)
			w.Header().Set("Content-Type", "application/json")
		}
		mux.HandleFunc(route, appHandler)
		_ = http.ListenAndServeTLS(listener, appCert, appKey, mux)
	}()
	if !assert.True(t, checkListenOrBail("https://"+path.Join(listener, route))) {
		err := fmt.Errorf("cannot connect to test https listener on: %s", "https://"+path.Join(listener, route))
		t.Logf("%v", err)
		t.FailNow()
		return err
	}
	t.Logf("test TLS app server: %s%s", listener, route)
	return nil
}

func runTestTLSConnect(t *testing.T, config *Config, listener, route string) (string, []*http.Cookie, error) {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs: makeTestCACertPool(),
		},
	}
	if err := http2.ConfigureTransport(transport); err != nil {
		return "", nil, err
	}

	client := http.Client{
		Transport: controlledRedirect{
			CollectedCookies: make(map[string]*http.Cookie, 10),
			Transport:        transport,
		},
		CheckRedirect: onRedirect,
	}
	u, _ := url.Parse("https://" + config.Listen + "/oauth/authorize")
	v := u.Query()
	v.Set("state", "my_client_nonce") // NOTE: this state provided by the client is not currently carried on to the end (lost)
	u.RawQuery = v.Encode()

	req := &http.Request{
		Method: "GET",
		URL:    u,
		Header: make(http.Header),
	}
	// add request_uri to specify last stop redirection (inner workings since PR #440)
	encoded := base64.StdEncoding.EncodeToString([]byte("https://" + listener + route))
	ck := &http.Cookie{
		Name:   "request_uri",
		Value:  encoded,
		Path:   "/",
		Secure: true,
	}
	req.AddCookie(ck)

	// attempts to login
	resp, err := client.Do(req)
	if !assert.NoError(t, err) {
		return "", nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	// check that we get the final redirection to app correctly
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	buf, erb := ioutil.ReadAll(resp.Body)
	assert.NoError(t, erb)
	assert.JSONEq(t, `{"message": "ok"}`, string(buf))

	// returns all collected cookies during the handshake
	collector := client.Transport.(controlledRedirect)
	collected := make([]*http.Cookie, 0, 10)
	for _, ck := range collector.CollectedCookies {
		collected = append(collected, ck)
	}

	// assert kc-access cookie
	var (
		found       bool
		accessToken string
	)
	for _, ck := range collected {
		if ck.Name == config.CookieAccessName {
			accessToken = ck.Value
			found = true
			break
		}
	}
	assert.True(t, found)
	if t.Failed() {
		return "", nil, errors.New("failed to connect")
	}
	return accessToken, collected, nil
}

func runTestTLSAuth(t *testing.T, listener, realm string) error {
	// a stub OIDC provider
	fake := newFakeAuthServer()
	fake.location, _ = url.Parse("https://" + listener)

	issuer := "https://" + listener + path.Join("/auth", "realms", realm)
	discoveryURL := testDiscoveryPath(realm)
	authorizeURL := path.Join("/auth", "realms", realm, "protocol", "openid-connect", "auth")
	// #nosec
	tokenURL := path.Join("/auth", "realms", realm, "protocol", "openid-connect", "token")
	jwksURL := path.Join("/auth", "realms", realm, "protocol", "openid-connect", "certs")

	go func() {
		mux := http.NewServeMux()
		configurationHandler := func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
				"issuer": "`+issuer+`",
				"subject_types_supported":["public","pairwise"],
				"id_token_signing_alg_values_supported":["ES384","RS384","HS256","HS512","ES256","RS256","HS384","ES512","RS512"],
				"userinfo_signing_alg_values_supported":["ES384","RS384","HS256","HS512","ES256","RS256","HS384","ES512","RS512","none"],
				"authorization_endpoint":"https://`+listener+authorizeURL+`",
				"token_endpoint":"https://`+listener+tokenURL+`",
				"jwks_uri":"https://`+listener+jwksURL+`"
			}`)
		}
		authorizeHandler := func(w http.ResponseWriter, req *http.Request) {
			redirect := req.FormValue("redirect_uri")
			state := req.FormValue("state")
			code := "zyx"
			location, _ := url.PathUnescape(redirect)
			u, _ := url.Parse(location)
			v := u.Query()
			v.Set("code", code)
			v.Set("state", state)
			u.RawQuery = v.Encode()
			http.Redirect(w, req, u.String(), http.StatusFound)
		}

		tokenHandler := func(w http.ResponseWriter, req *http.Request) {
			fake.tokenHandler(w, req)
		}

		keysHandler := func(w http.ResponseWriter, req *http.Request) {
			fake.keysHandler(w, req)
		}
		mux.HandleFunc(discoveryURL, configurationHandler)
		mux.HandleFunc(authorizeURL, authorizeHandler)
		mux.HandleFunc(tokenURL, tokenHandler)
		mux.HandleFunc(jwksURL, keysHandler)
		_ = http.ListenAndServeTLS(listener, authCert, authKey, mux)
	}()
	if !assert.True(t, checkListenOrBail("https://"+path.Join(listener, jwksURL))) {
		err := fmt.Errorf("cannot connect to test https listener on: %s", "https://"+path.Join(listener, jwksURL))
		t.Logf("%v", err)
		t.FailNow()
		return err
	}
	t.Logf("test TLS auth server: %s [%s]", listener, realm)
	return nil
}

func testTLSDiscoveryURL(listener, realm string) string {
	return "https://" + listener + testDiscoveryPath(realm)
}

func testBuildTLSUpstreamConfig() *Config {
	config := newDefaultConfig()
	config.Verbose = true
	config.EnableLogging = true
	config.DisableAllLogging = false

	config.Listen = e2eTLSUpstreamProxyListener
	config.ListenHTTP = ""
	config.DiscoveryURL = testTLSDiscoveryURL(e2eTLSUpstreamOauthListener, "hod-test")
	//config.SkipOpenIDProviderTLSVerify = true
	config.OpenIDProviderCA = caCert

	config.Upstream = "https://" + e2eTLSUpstreamUpstreamListener

	config.TLSCertificate = gkCert
	config.TLSPrivateKey = gkKey
	config.SkipUpstreamTLSVerify = false
	config.UpstreamCA = caCert
	config.TLSUseModernSettings = true

	config.CorsOrigins = []string{"*"}
	config.EnableCSRF = false
	config.HTTPOnlyCookie = false // since we want to inspect the cookie for testing
	config.SecureCookie = true
	config.AccessTokenDuration = 30 * time.Minute
	config.EnableEncryptedToken = false
	config.EnableSessionCookies = true
	config.EnableAuthorizationCookies = false
	config.EnableClaimsHeaders = false
	config.EnableTokenHeader = false
	config.EnableAuthorizationHeader = true
	config.ClientID = fakeClientID
	config.ClientSecret = fakeSecret
	config.Resources = []*Resource{
		{
			URL:           "/fake",
			Methods:       []string{"GET", "POST", "DELETE"},
			WhiteListed:   false,
			EnableCSRF:    false,
			Upstream:      "https://" + e2eTLSUpstreamUpstreamListener + e2eTLSUpstreamUpstreamURL,
			StripBasePath: "/fake",
		},
	}
	config.EncryptionKey = secretForCookie
	return config
}

func TestTLSUpstream(t *testing.T) {
	//log.SetOutput(ioutil.Discard)

	config := testBuildTLSUpstreamConfig()
	require.NoError(t, config.isValid())

	// launch fake oauth OIDC server (http for simplicity)
	err := runTestTLSAuth(t, e2eTLSUpstreamOauthListener, "hod-test")
	require.NoError(t, err)

	// launch fake upstream resource server
	err = runTestTLSUpstream(t, e2eTLSUpstreamUpstreamListener, e2eTLSUpstreamUpstreamURL, "mark1", "push", "mark2")
	require.NoError(t, err)

	// launch fake app server where to land after authentication
	err = runTestTLSApp(t, e2eTLSUpstreamAppListener, e2eTLSUpstreamAppURL)
	require.NoError(t, err)

	// launch keycloak-gatekeeper proxy
	err = runTestGatekeeper(t, config)
	require.NoError(t, err)

	// establish an authenticated session
	accessToken, cookies, err := runTestTLSConnect(t, config, e2eTLSUpstreamAppListener, e2eTLSUpstreamAppURL)
	require.NoErrorf(t, err, "could not login: %v", err)
	require.NotEmpty(t, accessToken)

	// scenario 1: routing to upstream, w/ TLS and HTTP/2
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    makeTestCACertPool(),
			NextProtos: []string{"h2", "http/1.1"},
		},
	}

	err = http2.ConfigureTransport(transport)
	require.NoError(t, err)

	// NOTE(fredbi): no support for client consuming http/2 pushes in http client
	// https://github.com/golang/go/issues/18594
	client := http.Client{
		Transport: transport,
	}

	h := make(http.Header, 10)
	h.Set("Content-Type", "application/json")
	h.Add("Accept", "application/json")

	// test TLS upstream
	u, _ := url.Parse("https://" + e2eTLSUpstreamProxyListener + "/fake")
	req := &http.Request{
		Method: "GET",
		URL:    u,
		Header: h,
	}
	copyCookies(req, cookies)

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() {
		_ = resp.Body.Close()
	}()

	dump, err := httputil.DumpResponse(resp, true)
	require.NoError(t, err)
	t.Logf("%q", dump)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	// also interactive test may produce HTTP/2 traces:
	// GODEBUG=http2debug=2 ; go test -v -run TLSUpstream
	assert.Equal(t, 2, resp.ProtoMajor) // assert response is HTTP/2
	buf, err := ioutil.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(buf), "mark1")
	t.Logf(string(buf))
}
