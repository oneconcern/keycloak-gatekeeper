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
	"context"
	"fmt"

	//"io/ioutil"
	//httplog "log"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"

	"net/http/httputil"

	"github.com/go-chi/chi"
	"github.com/rs/cors"
	"go.uber.org/zap"
)

// createReverseProxy creates a reverse proxy
func (r *oauthProxy) createReverseProxy() error {
	r.log.Info("enabled reverse proxy mode, default upstream url", zap.String("url", r.config.Upstream))
	if err := r.createStdProxy(r.endpoint); err != nil {
		return err
	}
	engine := chi.NewRouter()
	r.useDefaultStack(engine)

	// @step: configure CORS middleware
	if len(r.config.CorsOrigins) > 0 {
		c := cors.New(cors.Options{
			AllowedOrigins:   r.config.CorsOrigins,
			AllowedMethods:   r.config.CorsMethods,
			AllowedHeaders:   r.config.CorsHeaders,
			AllowCredentials: r.config.CorsCredentials,
			ExposedHeaders:   r.config.CorsExposedHeaders,
			MaxAge:           int(r.config.CorsMaxAge.Seconds()),
			Debug:            r.config.Verbose,
		})
		engine.Use(c.Handler)
	}

	r.router = engine

	if len(r.config.ResponseHeaders) > 0 {
		engine.Use(r.responseHeaderMiddleware(r.config.ResponseHeaders))
	}

	// configure CSRF middleware
	r.csrf = r.csrfConfigMiddleware()

	// step: add the handlers for oauth
	engine.With(
		proxyDenyMiddleware,
		r.csrfSkipMiddleware(), // handle CSRF state, but skip check on POST endpoints below
		r.csrfProtectMiddleware(),
		r.csrfHeaderMiddleware()).Route(r.config.OAuthURI,
		func(e chi.Router) {
			e.MethodNotAllowed(methodNotAllowedHandler)

			e.HandleFunc(authorizationURL, r.oauthAuthorizationHandler)
			e.Get(callbackURL, r.oauthCallbackHandler)
			e.Get(expiredURL, r.expirationHandler)

			e.With(r.authenticationMiddleware()).Get(logoutURL, r.logoutHandler)
			e.With(r.authenticationMiddleware()).Get(tokenURL, r.tokenHandler)

			e.Post(loginURL, r.loginHandler)

			if r.config.ListenAdmin == "" {
				e.Mount("/", r.createAdminRoutes())
			}
		})

	if r.config.ListenAdmin == "" {
		if debugEngine := r.createDebugRoutes(); debugEngine != nil {
			engine.With(proxyDenyMiddleware).Mount(debugURL, debugEngine)
		}
	}

	// step: load the templates if any
	if err := r.createTemplates(); err != nil {
		return err
	}
	// step: provision in the protected resources
	addDefaultDeny := r.config.EnableDefaultDeny
	for _, x := range r.config.Resources {
		if x.URL[len(x.URL)-1:] == "/" {
			r.log.Warn("the resource url is not a prefix",
				zap.String("resource", x.URL),
				zap.String("change", x.URL),
				zap.String("amended", strings.TrimRight(x.URL, "/")))
		}
		if x.URL == allRoutes && r.config.EnableDefaultDeny {
			addDefaultDeny = false
		}
	}

	// step: define expected behaviour on default route: "/*"
	if addDefaultDeny {
		if r.config.EnableDefaultNotFound {
			r.log.Info("routes which are not explicitly declared as resources will respond 401 not authenticated or 404 NotFound for authenticated users")
			engine.With(r.authenticationMiddleware()).
				Handle(allRoutes, http.HandlerFunc(methodNotFoundHandler))
		} else {
			r.log.Info("adding a default denial to protected resources: all routes to upstream require authentication")
			r.config.Resources = append(r.config.Resources, &Resource{URL: allRoutes, Methods: allHTTPMethods})
		}
	} else {
		if r.config.EnableDefaultNotFound {
			// this setting kicks in only on default catch all route, not if one has been explicitly set up
			foundAllRoutes := false
			for _, x := range r.config.Resources {
				if x.URL == allRoutes {
					foundAllRoutes = true
					break
				}
			}
			if !foundAllRoutes {
				r.log.Info("routes which are not explicitly declared as resources will respond 404 NotFound")
				engine.Handle(allRoutes, http.HandlerFunc(methodNotFoundHandler))
			}
		} else {
			r.log.Warn("routes to upstream are not configured to be denied by default")
			engine.With(r.proxyMiddleware(nil)).HandleFunc(allRoutes, emptyHandler)
		}
	}

	for _, x := range r.config.Resources {
		r.log.Info("protecting resource", zap.String("resource", x.String()))
		if !x.WhiteListed {
			e := engine.With(
				r.proxyMiddleware(x),
				r.authenticationMiddleware(),
				r.admissionMiddleware(x),
				r.identityHeadersMiddleware(r.config.AddClaims),
				r.csrfSkipResourceMiddleware(x),
				r.csrfProtectMiddleware(),
				r.csrfHeaderMiddleware())
			e.Handle(x.URL, http.HandlerFunc(methodNotAllowedHandler))
			for _, m := range x.Methods {
				e.MethodFunc(m, x.URL, emptyHandler)
			}
		} else {
			e := engine.With(
				r.proxyMiddleware(x))
			e.Handle(x.URL, http.HandlerFunc(methodNotAllowedHandler))
			for _, m := range x.Methods {
				e.MethodFunc(m, x.URL, emptyHandler)
			}
		}
	}

	// startup information

	if r.config.EnableSessionCookies {
		r.log.Info("using session cookies only for access and refresh tokens")
	}

	for name, value := range r.config.MatchClaims {
		r.log.Info("token must contain", zap.String("claim", name), zap.String("value", value))
	}
	if r.config.RedirectionURL == "" {
		r.log.Warn("no redirection url has been set, will use host headers")
	}
	if r.config.EnableEncryptedToken {
		r.log.Info("session access tokens will be encrypted")
	}

	return nil
}

// proxyMiddleware is responsible for handling reverse proxy request to the upstream endpoint
func (r *oauthProxy) proxyMiddleware(resource *Resource) func(http.Handler) http.Handler {
	var upstreamHost, upstreamScheme, upstreamBasePath, stripBasePath, matched string
	if resource != nil && resource.Upstream != "" {
		// resource-specific routing to upstream
		u, _ := url.Parse(resource.Upstream)
		matched = resource.URL
		upstreamHost = u.Host
		upstreamScheme = u.Scheme
		upstreamBasePath = u.Path
	} else {
		// default routing
		upstreamHost = r.endpoint.Host
		upstreamScheme = r.endpoint.Scheme
		upstreamBasePath = r.endpoint.Path
	}
	if resource != nil {
		stripBasePath = resource.StripBasePath
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req)

			// @step: retrieve the request scope
			scope := req.Context().Value(contextScopeName)
			if scope != nil {
				sc := scope.(*RequestScope)
				if sc.AccessDenied {
					return
				}
			}

			// @step: add the proxy forwarding headers
			req.Header.Add("X-Forwarded-For", realIP(req))
			req.Header.Set("X-Forwarded-Host", req.Host)
			if fp := req.Header.Get("X-Forwarded-Proto"); fp != "" {
				req.Header.Set("X-Forwarded-Proto", fp)
			} else {
				req.Header.Set("X-Forwarded-Proto", upstreamScheme)
			}

			if len(r.config.CorsOrigins) > 0 {
				// if CORS is enabled by gatekeeper, do not propagate CORS requests upstream
				req.Header.Del("Origin")
			}

			// @step: add any custom headers to the request
			for k, v := range r.config.Headers {
				req.Header.Set(k, v)
			}

			if r.config.EnableCSRF {
				// remove csrf header
				req.Header.Del(r.config.CSRFHeader)
				if !r.config.EnableAuthorizationCookies {
					_ = filterCookies(req, []string{requestURICookie, r.config.CSRFCookieName})
				}
			} else if !r.config.EnableAuthorizationCookies {
				_ = filterCookies(req, []string{requestURICookie})
			}

			req.URL.Host = upstreamHost
			req.URL.Scheme = upstreamScheme
			if stripBasePath != "" {
				// strip prefix if needed
				r.log.Debug("stripping prefix from URL", zap.String("stripBasePath", stripBasePath), zap.String("original_path", req.URL.Path))
				req.URL.Path = strings.TrimPrefix(req.URL.Path, stripBasePath)
			}
			if upstreamBasePath != "" {
				// add upstream URL component if any
				req.URL.Path = path.Join(upstreamBasePath, req.URL.Path)
			}

			// @note: by default goproxy only provides a forwarding proxy, thus all requests have to be absolute and we must update the host headers
			if v := req.Header.Get("Host"); v != "" {
				req.Host = v
				req.Header.Del("Host")
			} else if !r.config.PreserveHost {
				req.Host = upstreamHost
			}
			r.log.Debug("proxying to upstream", zap.String("matched_resource", matched), zap.Stringer("upstream_url", req.URL), zap.String("host_header", req.Host))

			if isUpgradedConnection(req) {
				r.log.Debug("upgrading the connnection", zap.String("client_ip", req.RemoteAddr))
				if err := tryUpdateConnection(req, w, req.URL); err != nil {
					r.errorResponse(w, "failed to upgrade connection", http.StatusInternalServerError, err)
					return
				}
				return
			}

			r.upstream.ServeHTTP(w, req)
		})
	}
}

// createStdProxy creates a reverse http proxy client to the upstream
// TODO: enable http2 support
// TODO:: multiple proxies with possibly different dialers and TLS configs
func (r *oauthProxy) createStdProxy(upstream *url.URL) error {
	dialer := (&net.Dialer{
		KeepAlive: r.config.UpstreamKeepaliveTimeout,
		Timeout:   r.config.UpstreamTimeout,
	}).DialContext

	// are we using a unix socket?
	if upstream != nil && upstream.Scheme == "unix" {
		r.log.Info("using unix socket for upstream", zap.String("socket", fmt.Sprintf("%s%s", upstream.Host, upstream.Path)))

		socketPath := fmt.Sprintf("%s%s", upstream.Host, upstream.Path)
		dialer = func(_ context.Context, network, address string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		}
		upstream.Path = ""
		upstream.Host = "domain-sock"
		upstream.Scheme = unsecureScheme
	}

	// create the upstream tls configuration
	tlsConfig, err := r.buildProxyTLSConfig()
	if err != nil {
		return err
	}

	r.upstream = &httputil.ReverseProxy{
		Director: func(*http.Request) {}, // most of the work is done by middleware. Some of this could be done by Director
		Transport: &http.Transport{
			DialContext:           dialer,
			TLSClientConfig:       tlsConfig,
			TLSHandshakeTimeout:   r.config.UpstreamTLSHandshakeTimeout,
			MaxIdleConns:          r.config.MaxIdleConns,
			MaxIdleConnsPerHost:   r.config.MaxIdleConnsPerHost,
			DisableKeepAlives:     !r.config.UpstreamKeepalives,
			ExpectContinueTimeout: r.config.UpstreamExpectContinueTimeout,
			ResponseHeaderTimeout: r.config.UpstreamResponseHeaderTimeout,
		},
	}

	return nil
}
