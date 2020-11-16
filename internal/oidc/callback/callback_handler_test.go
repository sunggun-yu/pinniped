// Copyright 2020 the Pinniped contributors. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package callback

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gorilla/securecookie"
	"github.com/stretchr/testify/require"

	"go.pinniped.dev/internal/oidc/provider"
	"go.pinniped.dev/internal/testutil"
)

const (
	happyUpstreamIDPName = "upstream-idp-name"
)

func TestCallbackEndpoint(t *testing.T) {
	upstreamAuthURL, err := url.Parse("https://some-upstream-idp:8443/auth")
	require.NoError(t, err)
	otherUpstreamAuthURL, err := url.Parse("https://some-other-upstream-idp:8443/auth")
	require.NoError(t, err)

	upstreamOIDCIdentityProvider := provider.UpstreamOIDCIdentityProvider{
		Name:             happyUpstreamIDPName,
		ClientID:         "some-client-id",
		AuthorizationURL: *upstreamAuthURL,
		Scopes:           []string{"scope1", "scope2"},
	}

	otherUpstreamOIDCIdentityProvider := provider.UpstreamOIDCIdentityProvider{
		Name:             "other-upstream-idp-name",
		ClientID:         "other-some-client-id",
		AuthorizationURL: *otherUpstreamAuthURL,
		Scopes:           []string{"other-scope1", "other-scope2"},
	}

	var stateEncoderHashKey = []byte("fake-hash-secret")
	var stateEncoderBlockKey = []byte("0123456789ABCDEF") // block encryption requires 16/24/32 bytes for AES
	var cookieEncoderHashKey = []byte("fake-hash-secret2")
	var cookieEncoderBlockKey = []byte("0123456789ABCDE2") // block encryption requires 16/24/32 bytes for AES
	require.NotEqual(t, stateEncoderHashKey, cookieEncoderHashKey)
	require.NotEqual(t, stateEncoderBlockKey, cookieEncoderBlockKey)

	var happyStateCodec = securecookie.New(stateEncoderHashKey, stateEncoderBlockKey)
	happyStateCodec.SetSerializer(securecookie.JSONEncoder{})
	var happyCookieCodec = securecookie.New(cookieEncoderHashKey, cookieEncoderBlockKey)
	happyCookieCodec.SetSerializer(securecookie.JSONEncoder{})

	happyCSRF := "test-csrf"
	happyPKCE := "test-pkce"
	happyNonce := "test-nonce"

	happyState, err := happyStateCodec.Encode("s",
		testutil.ExpectedUpstreamStateParamFormat{
			P: "todo query goes here",
			N: happyNonce,
			C: happyCSRF,
			K: happyPKCE,
			V: "1",
		},
	)
	require.NoError(t, err)

	wrongCSRFValueState, err := happyStateCodec.Encode("s",
		testutil.ExpectedUpstreamStateParamFormat{
			P: "todo query goes here",
			N: happyNonce,
			C: "wrong-csrf-value",
			K: happyPKCE,
			V: "1",
		},
	)
	require.NoError(t, err)

	wrongVersionState, err := happyStateCodec.Encode("s",
		testutil.ExpectedUpstreamStateParamFormat{
			P: "todo query goes here",
			N: happyNonce,
			C: happyCSRF,
			K: happyPKCE,
			V: "wrong-version",
		},
	)
	require.NoError(t, err)

	encodedIncomingCookieCSRFValue, err := happyCookieCodec.Encode("csrf", happyCSRF)
	require.NoError(t, err)
	happyCSRFCookie := "__Host-pinniped-csrf=" + encodedIncomingCookieCSRFValue

	tests := []struct {
		name string

		idpListGetter provider.DynamicUpstreamIDPProvider
		method        string
		path          string
		csrfCookie    string

		wantStatus int
		wantBody   string
	}{
		// Happy path
		// TODO: GET with good state and cookie and successful upstream token exchange and 302 to downstream client callback with its state and code

		// Pre-upstream-exchange verification
		{
			name:       "PUT method is invalid",
			method:     http.MethodPut,
			path:       newRequestPath().String(),
			wantStatus: http.StatusMethodNotAllowed,
			wantBody:   "Method Not Allowed: PUT (try GET)\n",
		},
		{
			name:       "POST method is invalid",
			method:     http.MethodPost,
			path:       newRequestPath().String(),
			wantStatus: http.StatusMethodNotAllowed,
			wantBody:   "Method Not Allowed: POST (try GET)\n",
		},
		{
			name:       "PATCH method is invalid",
			method:     http.MethodPatch,
			path:       newRequestPath().String(),
			wantStatus: http.StatusMethodNotAllowed,
			wantBody:   "Method Not Allowed: PATCH (try GET)\n",
		},
		{
			name:       "DELETE method is invalid",
			method:     http.MethodDelete,
			path:       newRequestPath().String(),
			wantStatus: http.StatusMethodNotAllowed,
			wantBody:   "Method Not Allowed: DELETE (try GET)\n",
		},
		{
			name:       "code param was not included on request",
			method:     http.MethodGet,
			path:       newRequestPath().WithState(happyState).WithoutCode().String(),
			csrfCookie: happyCSRFCookie,
			wantStatus: http.StatusBadRequest,
			wantBody:   "Bad Request: code param not found\n",
		},
		{
			name:       "state param was not included on request",
			method:     http.MethodGet,
			path:       newRequestPath().WithoutState().String(),
			csrfCookie: happyCSRFCookie,
			wantStatus: http.StatusBadRequest,
			wantBody:   "Bad Request: state param not found\n",
		},
		{
			name:          "state param was not signed correctly, has expired, or otherwise cannot be decoded for any reason",
			idpListGetter: testutil.NewIDPListGetter(upstreamOIDCIdentityProvider),
			method:        http.MethodGet,
			path:          newRequestPath().WithState("this-will-not-decode").String(),
			csrfCookie:    happyCSRFCookie,
			wantStatus:    http.StatusBadRequest,
			wantBody:      "Bad Request: error reading state\n",
		},
		{
			name:          "state's internal version does not match what we want",
			idpListGetter: testutil.NewIDPListGetter(upstreamOIDCIdentityProvider),
			method:        http.MethodGet,
			path:          newRequestPath().WithState(wrongVersionState).String(),
			csrfCookie:    happyCSRFCookie,
			wantStatus:    http.StatusUnprocessableEntity,
			wantBody:      "Unprocessable Entity: state format version is invalid\n",
		},
		{
			name:          "the UpstreamOIDCProvider CRD has been deleted",
			idpListGetter: testutil.NewIDPListGetter(otherUpstreamOIDCIdentityProvider),
			method:        http.MethodGet,
			path:          newRequestPath().WithState(happyState).String(),
			csrfCookie:    happyCSRFCookie,
			wantStatus:    http.StatusUnprocessableEntity,
			wantBody:      "Unprocessable Entity: upstream provider not found\n",
		},
		{
			name:          "the CSRF cookie does not exist on request",
			idpListGetter: testutil.NewIDPListGetter(otherUpstreamOIDCIdentityProvider),
			method:        http.MethodGet,
			path:          newRequestPath().WithState(happyState).String(),
			wantStatus:    http.StatusForbidden,
			wantBody:      "Forbidden: CSRF cookie is missing\n",
		},
		{
			name:          "cookie was not signed correctly, has expired, or otherwise cannot be decoded for any reason",
			idpListGetter: testutil.NewIDPListGetter(otherUpstreamOIDCIdentityProvider),
			method:        http.MethodGet,
			path:          newRequestPath().WithState(happyState).String(),
			csrfCookie:    "__Host-pinniped-csrf=this-value-was-not-signed-by-pinniped",
			wantStatus:    http.StatusForbidden,
			wantBody:      "Forbidden: error reading CSRF cookie\n",
		},
		{
			name:          "cookie csrf value does not match state csrf value",
			idpListGetter: testutil.NewIDPListGetter(otherUpstreamOIDCIdentityProvider),
			method:        http.MethodGet,
			path:          newRequestPath().WithState(wrongCSRFValueState).String(),
			csrfCookie:    happyCSRFCookie,
			wantStatus:    http.StatusForbidden,
			wantBody:      "Forbidden: CSRF value does not match\n",
		},

		// Upstream exchange
		// TODO: network call to upstream token endpoint fails
		// TODO: the upstream token endpoint returns an error

		// Post-upstream-exchange verification
		// TODO: returned tokens are invalid (all the stuff from the spec...)
		// TODO: there
		// TODO: are
		// TODO: probably
		// TODO: a
		// TODO: lot
		// TODO: of
		// TODO: test
		// TODO: cases
		// TODO: here (e.g., id jwt cannot be verified, nonce is wrong, we didn't get refresh token, we didn't get access token, we didn't get id token, access token expires too quickly)

		// Downstream redirect
		// TODO: we grant the openid scope if it was requested, similar to what we did in auth_handler.go
		// TODO: cannot generate auth code
		// TODO: cannot persist downstream state
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			subject := NewHandler(test.idpListGetter, happyStateCodec, happyCookieCodec)
			req := httptest.NewRequest(test.method, test.path, nil)
			if test.csrfCookie != "" {
				req.Header.Set("Cookie", test.csrfCookie)
			}
			rsp := httptest.NewRecorder()
			subject.ServeHTTP(rsp, req)

			require.Equal(t, test.wantStatus, rsp.Code)
			require.Equal(t, test.wantBody, rsp.Body.String())
		})
	}
}

type requestPath struct {
	upstreamIDPName, code, state *string
}

func newRequestPath() *requestPath {
	n := happyUpstreamIDPName
	c := "1234"
	s := "4321"
	return &requestPath{
		upstreamIDPName: &n,
		code:            &c,
		state:           &s,
	}
}

func (r *requestPath) WithUpstreamIDPName(name string) *requestPath {
	r.upstreamIDPName = &name
	return r
}

func (r *requestPath) WithCode(code string) *requestPath {
	r.code = &code
	return r
}

func (r *requestPath) WithoutCode() *requestPath {
	r.code = nil
	return r
}

func (r *requestPath) WithState(state string) *requestPath {
	r.state = &state
	return r
}

func (r *requestPath) WithoutState() *requestPath {
	r.state = nil
	return r
}

func (r *requestPath) String() string {
	path := fmt.Sprintf("/downstream-provider-name/callback/%s?", *r.upstreamIDPName)
	params := url.Values{}
	if r.code != nil {
		params.Add("code", *r.code)
	}
	if r.state != nil {
		params.Add("state", *r.state)
	}
	return path + params.Encode()
}
