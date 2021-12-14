// Copyright 2020-2021 the Pinniped contributors. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package upstreamoidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"gopkg.in/square/go-jose.v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.pinniped.dev/internal/mocks/mockkeyset"
	"go.pinniped.dev/internal/testutil"
	"go.pinniped.dev/pkg/oidcclient/nonce"
	"go.pinniped.dev/pkg/oidcclient/oidctypes"
)

func TestProviderConfig(t *testing.T) {
	t.Run("getters get", func(t *testing.T) {
		p := ProviderConfig{
			Name:          "test-name",
			UsernameClaim: "test-username-claim",
			GroupsClaim:   "test-groups-claim",
			Config: &oauth2.Config{
				ClientID: "test-client-id",
				Endpoint: oauth2.Endpoint{AuthURL: "https://example.com"},
				Scopes:   []string{"scope1", "scope2"},
			},
		}
		require.Equal(t, "test-name", p.GetName())
		require.Equal(t, "test-client-id", p.GetClientID())
		require.Equal(t, "https://example.com", p.GetAuthorizationURL().String())
		require.ElementsMatch(t, []string{"scope1", "scope2"}, p.GetScopes())
		require.Equal(t, "test-username-claim", p.GetUsernameClaim())
		require.Equal(t, "test-groups-claim", p.GetGroupsClaim())

		// AllowPasswordGrant defaults to false.
		require.False(t, p.AllowsPasswordGrant())
		p.AllowPasswordGrant = true
		require.True(t, p.AllowsPasswordGrant())
		p.AllowPasswordGrant = false
		require.False(t, p.AllowsPasswordGrant())
	})

	const (
		// Test JWTs generated with https://smallstep.com/docs/cli/crypto/jwt/:

		// step crypto keypair key.pub key.priv --kty RSA --no-password --insecure --force && echo '{"at_hash": "invalid-at-hash"}' | step crypto jwt sign --key key.priv --aud test-client-id --sub test-user --subtle --kid="test-kid" --jti="test-jti"
		invalidAccessTokenHashIDToken = "eyJhbGciOiJSUzI1NiIsImtpZCI6InRlc3Qta2lkIiwidHlwIjoiSldUIn0.eyJhdF9oYXNoIjoiaW52YWxpZC1hdC1oYXNoIiwiYXVkIjoidGVzdC1jbGllbnQtaWQiLCJpYXQiOjE2MDIyODM3OTEsImp0aSI6InRlc3QtanRpIiwibmJmIjoxNjAyMjgzNzkxLCJzdWIiOiJ0ZXN0LXVzZXIifQ.jryXr4jiwcf79wBLaHpjdclEYHoUFGhvTu95QyA6Hnk9NQ0x1vsWYurtj7a8uKydNPryC_HNZi9QTAE_tRIJjycseog3695-5y4B4EZlqL-a94rdOtffuF2O_lnPbKvoja9EKNrp0kLBCftFRHhLAEwuP0N9E5padZwPpIGK0yE_JqljnYgCySvzsQu7tasR38yaULny13h3mtp2WRHPG5DrLyuBuF8Z01hSgRi5hGcVpgzTwBgV5-eMaSUCUo-ZDkqUsLQI6dVlaikCSKYZRb53HeexH0tB_R9PJJHY7mIr-rS76kkQEx9pLuVnheIH9Oc6zbdYWg-zWMijopA8Pg" //nolint: gosec

		// step crypto keypair key.pub key.priv --kty RSA --no-password --insecure --force && echo '{"nonce": "invalid-nonce"}' | step crypto jwt sign --key key.priv --aud test-client-id --sub test-user --subtle --kid="test-kid" --jti="test-jti"
		invalidNonceIDToken = "eyJhbGciOiJSUzI1NiIsImtpZCI6InRlc3Qta2lkIiwidHlwIjoiSldUIn0.eyJhdWQiOiJ0ZXN0LWNsaWVudC1pZCIsImlhdCI6MTYwMjI4Mzc0MSwianRpIjoidGVzdC1qdGkiLCJuYmYiOjE2MDIyODM3NDEsIm5vbmNlIjoiaW52YWxpZC1ub25jZSIsInN1YiI6InRlc3QtdXNlciJ9.PRpq-7j5djaIAkraL-8t8ad9Xm4hM8RW67gyD1VIe0BecWeBFxsTuh3SZVKM9zmcwTgjudsyn8kQOwipDa49IN4PV8FcJA_uUJZi2wiqGJUSTG2K5I89doV_7e0RM1ZYIDDW1G2heKJNW7MbKkX7iEPr7u4MyEzswcPcupbyDA-CQFeL95vgwawoqa6yO94ympTbozqiNfj6Xyw_nHtThQnstjWsJZ9s2mUgppZezZv4HZYTQ7c3e_bzwhWgCzh2CSDJn9_Ra_n_4GcVkpHbsHTP35dFsnf0vactPx6CAu6A1-Apk-BruCktpZ3B4Ercf1UnUOHdGqzQKJtqvB03xQ" //nolint: gosec

		// step crypto keypair key.pub key.priv --kty RSA --no-password --insecure --force && echo '{"foo": "bar", "bat": "baz"}' | step crypto jwt sign --key key.priv --aud test-client-id --sub '' --subtle --kid="test-kid" --jti="test-jti"
		invalidSubClaim = "eyJhbGciOiJSUzI1NiIsImtpZCI6InRlc3Qta2lkIiwidHlwIjoiSldUIn0.eyJhdWQiOiJ0ZXN0LWNsaWVudC1pZCIsImJhdCI6ImJheiIsImZvbyI6ImJhciIsImlhdCI6MTYxMDIxOTY5MCwianRpIjoidGVzdC1qdGkiLCJuYmYiOjE2MTAyMTk2OTB9.CXgUarh9A8QByF_ddw0W1Cldl_n1qmry2cZh9U0Avi5sl7hb1y22MadDLQslvnx0NKx6EdbwI-El7QxDy0SzwomJomFL7WNd5gGk-Ilq9O_emaHekbpphZ5kxyudsAGUYGxrg1zysv1k5JPhnLnOUMcE7wa0uPLDWnrlAMzqHvnbjI3lakZ8v4-dfAKUIUGi3ycwuAh9BdpydwAsSNOpGBM55-O8911dqVfZKiFNNUeHYE1qlnbhCz7_ykLrljao0nRBbEf9FXGolCdhIaglt0LtaZvll9T9StIbSpcRaBGuRm8toTezmhmHjU-iCc0jGeVKsp8eTyOuJllqDSS-uw"

		// step crypto keypair key.pub key.priv --kty RSA --no-password --insecure --force && echo '{"foo": "bar", "bat": "baz"}' | step crypto jwt sign --key key.priv --aud test-client-id --sub test-user --subtle --kid="test-kid" --jti="test-jti"
		validIDToken = "eyJhbGciOiJSUzI1NiIsImtpZCI6InRlc3Qta2lkIiwidHlwIjoiSldUIn0.eyJhdWQiOiJ0ZXN0LWNsaWVudC1pZCIsImJhdCI6ImJheiIsImZvbyI6ImJhciIsImlhdCI6MTYwNjc2ODU5MywianRpIjoidGVzdC1qdGkiLCJuYmYiOjE2MDY3Njg1OTMsInN1YiI6InRlc3QtdXNlciJ9.DuqVZ7pGhHqKz7gNr4j2W1s1N8YrSltktH4wW19L4oD1OE2-O72jAnNj5xdjilsa8l7h9ox-5sMF0Tkh3BdRlHQK9dEtNm9tW-JreUnWJ3LCqUs-LZp4NG7edvq2sH_1Bn7O2_NQV51s8Pl04F60CndjQ4NM-6WkqDQTKyY6vJXU7idvM-6TM2HJZK-Na88cOJ9KIK37tL5DhcbsHVF47Dq8uPZ0KbjNQjJLAIi_1GeQBgc6yJhDUwRY4Xu6S0dtTHA6xTI8oSXoamt4bkViEHfJBp97LZQiNz8mku5pVc0aNwP1p4hMHxRHhLXrJjbh-Hx4YFjxtOnIq9t1mHlD4A" //nolint: gosec
	)

	t.Run("PasswordCredentialsGrantAndValidateTokens", func(t *testing.T) {
		tests := []struct {
			name                  string
			disallowPasswordGrant bool
			returnIDTok           string
			tokenStatusCode       int
			wantErr               string
			wantToken             oidctypes.Token

			rawClaims          []byte
			userInfo           *oidc.UserInfo
			userInfoErr        error
			wantUserInfoCalled bool
		}{
			{
				name:        "valid",
				returnIDTok: validIDToken,
				wantToken: oidctypes.Token{
					AccessToken: &oidctypes.AccessToken{
						Token:  "test-access-token",
						Expiry: metav1.Time{},
					},
					RefreshToken: &oidctypes.RefreshToken{
						Token: "test-refresh-token",
					},
					IDToken: &oidctypes.IDToken{
						Token:  validIDToken,
						Expiry: metav1.Time{},
						Claims: map[string]interface{}{
							"foo": "bar",
							"bat": "baz",
							"aud": "test-client-id",
							"iat": 1.606768593e+09,
							"jti": "test-jti",
							"nbf": 1.606768593e+09,
							"sub": "test-user",
						},
					},
				},
				rawClaims:          []byte(`{}`), // user info not supported
				wantUserInfoCalled: false,
			},
			{
				name:        "valid with userinfo",
				returnIDTok: validIDToken,
				wantToken: oidctypes.Token{
					AccessToken: &oidctypes.AccessToken{
						Token:  "test-access-token",
						Expiry: metav1.Time{},
					},
					RefreshToken: &oidctypes.RefreshToken{
						Token: "test-refresh-token",
					},
					IDToken: &oidctypes.IDToken{
						Token:  validIDToken,
						Expiry: metav1.Time{},
						Claims: map[string]interface{}{
							"foo":    "awesomeness", // overwrite existing claim
							"bat":    "baz",
							"aud":    "test-client-id",
							"iat":    1.606768593e+09,
							"jti":    "test-jti",
							"nbf":    1.606768593e+09,
							"sub":    "test-user",
							"groups": "fancy-group", // add a new claim
						},
					},
				},
				// claims is private field so we have to use hacks to set it
				userInfo:           forceUserInfoWithClaims("test-user", `{"foo":"awesomeness","groups":"fancy-group"}`),
				wantUserInfoCalled: true,
			},
			{
				name:                  "password grant not allowed",
				disallowPasswordGrant: true, // password grant is not allowed in this ProviderConfig
				wantErr:               "resource owner password credentials grant is not allowed for this upstream provider according to its configuration",
			},
			{
				name:            "token request fails with http error",
				tokenStatusCode: http.StatusForbidden,
				wantErr:         "oauth2: cannot fetch token: 403 Forbidden\nResponse: fake error\n",
			},
			{
				name:    "missing ID token",
				wantErr: "received response missing ID token",
			},
			{
				name:        "invalid ID token",
				returnIDTok: "invalid-jwt",
				wantErr:     "received invalid ID token: oidc: malformed jwt: square/go-jose: compact JWS format must have three parts",
			},
			{
				name:        "invalid access token hash",
				returnIDTok: invalidAccessTokenHashIDToken,
				wantErr:     "received invalid ID token: access token hash does not match value in ID token",
			},
			{
				name:        "user info fetch error",
				returnIDTok: validIDToken,
				wantErr:     "could not fetch user info claims: could not get user info: some network error",
				userInfoErr: errors.New("some network error"),
			},
			{
				name:        "user info sub error",
				returnIDTok: validIDToken,
				wantErr:     "could not fetch user info claims: userinfo 'sub' claim (test-user-2) did not match id_token 'sub' claim (test-user)",
				userInfo:    &oidc.UserInfo{Subject: "test-user-2"},
			},
			{
				name:        "user info is not json",
				returnIDTok: validIDToken,
				wantErr:     "could not fetch user info claims: could not unmarshal user info claims: invalid character 'i' looking for beginning of value",
				// claims is private field so we have to use hacks to set it
				userInfo: forceUserInfoWithClaims("test-user", `invalid-json-data`),
			},
			{
				name:        "invalid sub claim",
				returnIDTok: invalidSubClaim,
				wantToken: oidctypes.Token{
					AccessToken: &oidctypes.AccessToken{
						Token:  "test-access-token",
						Expiry: metav1.Time{},
					},
					RefreshToken: &oidctypes.RefreshToken{
						Token: "test-refresh-token",
					},
					IDToken: &oidctypes.IDToken{
						Token:  invalidSubClaim,
						Expiry: metav1.Time{},
						Claims: map[string]interface{}{
							"foo": "bar",
							"bat": "baz",
							"aud": "test-client-id",
							"iat": 1.61021969e+09,
							"jti": "test-jti",
							"nbf": 1.61021969e+09,
							// no sub claim
						},
					},
				},
				wantUserInfoCalled: false,
			},
		}
		for _, tt := range tests {
			tt := tt
			t.Run(tt.name, func(t *testing.T) {
				tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, http.MethodPost, r.Method)
					require.NoError(t, r.ParseForm())
					require.Equal(t, 6, len(r.Form))
					require.Equal(t, "password", r.Form.Get("grant_type"))
					require.Equal(t, "test-client-id", r.Form.Get("client_id"))
					require.Equal(t, "test-client-secret", r.Form.Get("client_secret"))
					require.Equal(t, "test-username", r.Form.Get("username"))
					require.Equal(t, "test-password", r.Form.Get("password"))
					require.Equal(t, "scope1 scope2", r.Form.Get("scope"))
					if tt.tokenStatusCode != 0 {
						http.Error(w, "fake error", http.StatusForbidden)
						return
					}
					var response struct {
						oauth2.Token
						IDToken string `json:"id_token,omitempty"`
					}
					response.AccessToken = "test-access-token"
					response.RefreshToken = "test-refresh-token"
					response.Expiry = time.Now().Add(time.Hour)
					response.IDToken = tt.returnIDTok
					w.Header().Set("content-type", "application/json")
					require.NoError(t, json.NewEncoder(w).Encode(&response))
				}))
				t.Cleanup(tokenServer.Close)

				rawClaims := tt.rawClaims
				if len(rawClaims) == 0 && (tt.userInfo != nil || tt.userInfoErr != nil) {
					rawClaims = []byte(`{"userinfo_endpoint": "not-empty"}`)
				}

				p := ProviderConfig{
					Name:          "test-name",
					UsernameClaim: "test-username-claim",
					GroupsClaim:   "test-groups-claim",
					Config: &oauth2.Config{
						ClientID:     "test-client-id",
						ClientSecret: "test-client-secret",
						Endpoint: oauth2.Endpoint{
							AuthURL:   "https://example.com",
							TokenURL:  tokenServer.URL,
							AuthStyle: oauth2.AuthStyleInParams,
						},
						Scopes: []string{"scope1", "scope2"},
					},
					Provider: &mockProvider{
						rawClaims:   rawClaims,
						userInfo:    tt.userInfo,
						userInfoErr: tt.userInfoErr,
					},
					AllowPasswordGrant: !tt.disallowPasswordGrant,
				}

				tok, err := p.PasswordCredentialsGrantAndValidateTokens(
					context.Background(),
					"test-username",
					"test-password",
				)

				if tt.wantErr != "" {
					require.EqualError(t, err, tt.wantErr)
					require.Nil(t, tok)
					return
				}
				require.NoError(t, err)
				require.Equal(t, &tt.wantToken, tok)
				require.Equal(t, tt.wantUserInfoCalled, p.Provider.(*mockProvider).called)
			})
		}
	})

	t.Run("PerformRefresh", func(t *testing.T) {
		tests := []struct {
			name             string
			returnIDTok      string
			returnAccessTok  string
			returnRefreshTok string
			returnTokType    string
			returnExpiresIn  string
			tokenStatusCode  int

			wantErr         string
			wantToken       *oauth2.Token
			wantTokenExtras map[string]interface{}
		}{
			{
				name:             "success when the server returns all tokens in the refresh result",
				returnIDTok:      "test-id-token",
				returnAccessTok:  "test-access-token",
				returnRefreshTok: "test-refresh-token",
				returnTokType:    "test-token-type",
				returnExpiresIn:  "42",
				tokenStatusCode:  http.StatusOK,
				wantToken: &oauth2.Token{
					AccessToken:  "test-access-token",
					RefreshToken: "test-refresh-token",
					TokenType:    "test-token-type",
					Expiry:       time.Now().Add(42 * time.Second),
				},
				wantTokenExtras: map[string]interface{}{
					// the ID token only appears in the extras map
					"id_token": "test-id-token",
					// the library also repeats all the other keys/values returned by the server in the raw extras map
					"access_token":  "test-access-token",
					"refresh_token": "test-refresh-token",
					"token_type":    "test-token-type",
					"expires_in":    "42",
					// the library also adds this zero-value even though the server did not return it
					"expiry": "0001-01-01T00:00:00Z",
				},
			},
			{
				name:             "success when the server does not return a new refresh token in the refresh result",
				returnIDTok:      "test-id-token",
				returnAccessTok:  "test-access-token",
				returnRefreshTok: "",
				returnTokType:    "test-token-type",
				returnExpiresIn:  "42",
				tokenStatusCode:  http.StatusOK,
				wantToken: &oauth2.Token{
					AccessToken: "test-access-token",
					// the library sets the original refresh token into the result, even though the server did not return that
					RefreshToken: "test-initial-refresh-token",
					TokenType:    "test-token-type",
					Expiry:       time.Now().Add(42 * time.Second),
				},
				wantTokenExtras: map[string]interface{}{
					// the ID token only appears in the extras map
					"id_token": "test-id-token",
					// the library also repeats all the other keys/values returned by the server in the raw extras map
					"access_token": "test-access-token",
					"token_type":   "test-token-type",
					"expires_in":   "42",
					// the library also adds this zero-value even though the server did not return it
					"expiry": "0001-01-01T00:00:00Z",
				},
			},
			{
				name:             "success when the server does not return a new ID token in the refresh result",
				returnIDTok:      "",
				returnAccessTok:  "test-access-token",
				returnRefreshTok: "test-refresh-token",
				returnTokType:    "test-token-type",
				returnExpiresIn:  "42",
				tokenStatusCode:  http.StatusOK,
				wantToken: &oauth2.Token{
					AccessToken:  "test-access-token",
					RefreshToken: "test-refresh-token",
					TokenType:    "test-token-type",
					Expiry:       time.Now().Add(42 * time.Second),
				},
				wantTokenExtras: map[string]interface{}{
					// the library also repeats all the other keys/values returned by the server in the raw extras map
					"access_token":  "test-access-token",
					"refresh_token": "test-refresh-token",
					"token_type":    "test-token-type",
					"expires_in":    "42",
					// the library also adds this zero-value even though the server did not return it
					"expiry": "0001-01-01T00:00:00Z",
				},
			},
			{
				name:            "server returns an error on token refresh",
				tokenStatusCode: http.StatusForbidden,
				wantErr:         "oauth2: cannot fetch token: 403 Forbidden\nResponse: fake error\n",
			},
		}
		for _, tt := range tests {
			tt := tt
			t.Run(tt.name, func(t *testing.T) {
				tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, http.MethodPost, r.Method)
					require.NoError(t, r.ParseForm())
					require.Equal(t, 4, len(r.Form))
					require.Equal(t, "test-client-id", r.Form.Get("client_id"))
					require.Equal(t, "test-client-secret", r.Form.Get("client_secret"))
					require.Equal(t, "refresh_token", r.Form.Get("grant_type"))
					require.Equal(t, "test-initial-refresh-token", r.Form.Get("refresh_token"))
					if tt.tokenStatusCode != http.StatusOK {
						http.Error(w, "fake error", tt.tokenStatusCode)
						return
					}
					var response struct {
						oauth2.Token
						IDToken   string `json:"id_token,omitempty"`
						ExpiresIn string `json:"expires_in,omitempty"`
					}
					response.IDToken = tt.returnIDTok
					response.AccessToken = tt.returnAccessTok
					response.RefreshToken = tt.returnRefreshTok
					response.TokenType = tt.returnTokType
					response.ExpiresIn = tt.returnExpiresIn
					w.Header().Set("content-type", "application/json")
					require.NoError(t, json.NewEncoder(w).Encode(&response))
				}))
				t.Cleanup(tokenServer.Close)

				p := ProviderConfig{
					Name:          "test-name",
					UsernameClaim: "test-username-claim",
					GroupsClaim:   "test-groups-claim",
					Config: &oauth2.Config{
						ClientID:     "test-client-id",
						ClientSecret: "test-client-secret",
						Endpoint: oauth2.Endpoint{
							AuthURL:   "https://example.com",
							TokenURL:  tokenServer.URL,
							AuthStyle: oauth2.AuthStyleInParams,
						},
						Scopes: []string{"scope1", "scope2"},
					},
				}

				tok, err := p.PerformRefresh(
					context.Background(),
					"test-initial-refresh-token",
				)

				if tt.wantErr != "" {
					require.EqualError(t, err, tt.wantErr)
					require.Nil(t, tok)
					return
				}

				require.NoError(t, err)
				require.Equal(t, tt.wantToken.TokenType, tok.TokenType)
				require.Equal(t, tt.wantToken.RefreshToken, tok.RefreshToken)
				require.Equal(t, tt.wantToken.AccessToken, tok.AccessToken)
				testutil.RequireTimeInDelta(t, tt.wantToken.Expiry, tok.Expiry, 5*time.Second)
				for k, v := range tt.wantTokenExtras {
					require.Equal(t, v, tok.Extra(k))
				}
			})
		}
	})

	t.Run("RevokeRefreshToken", func(t *testing.T) {
		tests := []struct {
			name             string
			nilRevocationURL bool
			statusCodes      []int
			returnErrBodies  []string
			wantErr          string
			wantNumRequests  int
		}{
			{
				name:             "success without calling the server when there is no revocation URL set",
				nilRevocationURL: true,
				wantNumRequests:  0,
			},
			{
				name:            "success when the server returns 200 OK on the first call",
				statusCodes:     []int{http.StatusOK},
				wantNumRequests: 1,
			},
			{
				name:        "success when the server returns 400 Bad Request on the first call due to client auth, then 200 OK on second call",
				statusCodes: []int{http.StatusBadRequest, http.StatusOK},
				// https://datatracker.ietf.org/doc/html/rfc6749#section-5.2 defines this as the error for client auth failure
				returnErrBodies: []string{`{ "error":"invalid_client", "error_description":"unhappy" }`},
				wantNumRequests: 2,
			},
			{
				name:            "error when the server returns 400 Bad Request on the first call due to client auth, then any 400 error on second call",
				statusCodes:     []int{http.StatusBadRequest, http.StatusBadRequest},
				returnErrBodies: []string{`{ "error":"invalid_client", "error_description":"unhappy" }`, `{ "error":"anything", "error_description":"unhappy" }`},
				wantErr:         `server responded with status 400 with body: { "error":"anything", "error_description":"unhappy" }`,
				wantNumRequests: 2,
			},
			{
				name:            "error when the server returns 400 Bad Request with bad JSON body on the first call",
				statusCodes:     []int{http.StatusBadRequest},
				returnErrBodies: []string{`invalid JSON body`},
				wantErr:         `error parsing response body "invalid JSON body" on response with status code 400: invalid character 'i' looking for beginning of value`,
				wantNumRequests: 1,
			},
			{
				name:            "error when the server returns 400 Bad Request with empty body",
				statusCodes:     []int{http.StatusBadRequest},
				returnErrBodies: []string{``},
				wantErr:         `error parsing response body "" on response with status code 400: unexpected end of JSON input`,
				wantNumRequests: 1,
			},
			{
				name:            "error when the server returns 400 Bad Request on the first call due to client auth, then any other error on second call",
				statusCodes:     []int{http.StatusBadRequest, http.StatusForbidden},
				returnErrBodies: []string{`{ "error":"invalid_client", "error_description":"unhappy" }`, ""},
				wantErr:         "server responded with status 403",
				wantNumRequests: 2,
			},
			{
				name:            "error when server returns any other 400 error on first call",
				statusCodes:     []int{http.StatusBadRequest},
				returnErrBodies: []string{`{ "error":"anything_else", "error_description":"unhappy" }`},
				wantErr:         `server responded with status 400 with body: { "error":"anything_else", "error_description":"unhappy" }`,
				wantNumRequests: 1,
			},
			{
				name:            "error when server returns any other error aside from 400 on first call",
				statusCodes:     []int{http.StatusForbidden},
				returnErrBodies: []string{""},
				wantErr:         "server responded with status 403",
				wantNumRequests: 1,
			},
		}
		for _, tt := range tests {
			tt := tt
			numRequests := 0
			t.Run(tt.name, func(t *testing.T) {
				tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					numRequests++
					require.LessOrEqual(t, numRequests, 2)
					require.Equal(t, http.MethodPost, r.Method)
					require.NoError(t, r.ParseForm())
					if numRequests == 1 {
						// First request should use client_id/client_secret params.
						require.Equal(t, 4, len(r.Form))
						require.Equal(t, "test-client-id", r.Form.Get("client_id"))
						require.Equal(t, "test-client-secret", r.Form.Get("client_secret"))
						require.Equal(t, "refresh_token", r.Form.Get("token_type_hint"))
						require.Equal(t, "test-initial-refresh-token", r.Form.Get("token"))
					} else {
						// Second request, if there is one, should use basic auth.
						require.Equal(t, 2, len(r.Form))
						require.Equal(t, "refresh_token", r.Form.Get("token_type_hint"))
						require.Equal(t, "test-initial-refresh-token", r.Form.Get("token"))
						username, password, hasBasicAuth := r.BasicAuth()
						require.True(t, hasBasicAuth, "request should have had basic auth but did not")
						require.Equal(t, "test-client-id", username)
						require.Equal(t, "test-client-secret", password)
					}
					if tt.statusCodes[numRequests-1] != http.StatusOK {
						w.Header().Set("content-type", "application/json")
						http.Error(w, tt.returnErrBodies[numRequests-1], tt.statusCodes[numRequests-1])
					}
					// Otherwise, responds with 200 OK and empty body by default.
				}))
				t.Cleanup(tokenServer.Close)

				tokenURL, err := url.Parse(tokenServer.URL)
				require.NoError(t, err)

				p := ProviderConfig{
					Name: "test-name",
					Config: &oauth2.Config{
						ClientID:     "test-client-id",
						ClientSecret: "test-client-secret",
					},
					RevocationURL: tokenURL,
					Client:        http.DefaultClient,
				}
				if tt.nilRevocationURL {
					p.RevocationURL = nil
				}

				err = p.RevokeRefreshToken(
					context.Background(),
					"test-initial-refresh-token",
				)

				require.Equal(t, tt.wantNumRequests, numRequests,
					"did not make expected number of requests to revocation endpoint")

				if tt.wantErr != "" {
					require.EqualError(t, err, tt.wantErr)
				}
			})
		}
	})

	t.Run("ValidateToken", func(t *testing.T) {
		expiryTime := time.Now().Add(42 * time.Second)
		testTokenWithoutIDToken := &oauth2.Token{
			AccessToken: "test-access-token",
			// the library sets the original refresh token into the result, even though the server did not return that
			RefreshToken: "test-initial-refresh-token",
			TokenType:    "test-token-type",
			Expiry:       expiryTime,
		}
		// generated from jwt.io
		// sub: some-subject
		// iss: some-issuer
		// nonce: some-nonce
		goodIDToken := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJzb21lLXN1YmplY3QiLCJub25jZSI6InNvbWUtbm9uY2UiLCJpc3MiOiJzb21lLWlzc3VlciJ9.eGvzOihLUqzn3M4k6fHsToedgy7Fu89_Xu_u4mwMgRlIyRWZqmEMV76RVLnZd9Ihm9j_VpvrpirIkaj4JM9eRNfLX1n328cmBivBwnTKAzHuTm17dUKO5EvdTmQzmwnN0WZ8nWk4GfR7RzcvE1V8G9tIiWD8FkO3Dr-NR_zTun3N37onAazVLCmF0SDtATDfUH1ETqviHEp8xGx5HD5mv5T3HEjOuer5gxTEnfncef0LurBH3po-C0tXHKu74PD8x88CMJ1DLsRdCalnctwa850slKPkBSTP-ssh0JVg7cdMXoosVpwiXtKYaBkrhu8VS018aFP-cBbW0mYwsHmt3g" //nolint:gosec

		tests := []struct {
			name             string
			tok              *oauth2.Token
			nonce            nonce.Nonce
			requireIDToken   bool
			userInfo         *oidc.UserInfo
			rawClaims        []byte
			userInfoErr      error
			wantErr          string
			wantMergedTokens *oidctypes.Token
		}{
			{
				name:           "token with id, access and refresh tokens, valid nonce, and no userinfo",
				tok:            testTokenWithoutIDToken.WithExtra(map[string]interface{}{"id_token": goodIDToken}),
				nonce:          "some-nonce",
				requireIDToken: true,
				rawClaims:      []byte(`{"userinfo_endpoint": "not-empty"}`),
				wantMergedTokens: &oidctypes.Token{
					AccessToken: &oidctypes.AccessToken{
						Token:  "test-access-token",
						Type:   "test-token-type",
						Expiry: metav1.NewTime(expiryTime),
					},
					RefreshToken: &oidctypes.RefreshToken{
						Token: "test-initial-refresh-token",
					},
					IDToken: &oidctypes.IDToken{
						Token: goodIDToken,
						Claims: map[string]interface{}{
							"iss":   "some-issuer",
							"nonce": "some-nonce",
							"sub":   "some-subject",
						},
					},
				},
			},
			{
				name:           "id token not required but is provided",
				tok:            testTokenWithoutIDToken.WithExtra(map[string]interface{}{"id_token": goodIDToken}),
				nonce:          "some-nonce",
				requireIDToken: false,
				rawClaims:      []byte(`{"userinfo_endpoint": "not-empty"}`),
				userInfo:       forceUserInfoWithClaims("some-subject", `{"name": "Pinny TheSeal"}`),
				wantMergedTokens: &oidctypes.Token{
					AccessToken: &oidctypes.AccessToken{
						Token:  "test-access-token",
						Type:   "test-token-type",
						Expiry: metav1.NewTime(expiryTime),
					},
					RefreshToken: &oidctypes.RefreshToken{
						Token: "test-initial-refresh-token",
					},
					IDToken: &oidctypes.IDToken{
						Token: goodIDToken,
						Claims: map[string]interface{}{
							"iss":   "some-issuer",
							"nonce": "some-nonce",
							"sub":   "some-subject",
							"name":  "Pinny TheSeal",
						},
					},
				},
			},
			{
				name:           "token with id, access and refresh tokens, valid nonce, and userinfo with a value that doesn't exist in the id token",
				tok:            testTokenWithoutIDToken.WithExtra(map[string]interface{}{"id_token": goodIDToken}),
				nonce:          "some-nonce",
				requireIDToken: true,
				rawClaims:      []byte(`{"userinfo_endpoint": "not-empty"}`),
				userInfo:       forceUserInfoWithClaims("some-subject", `{"name": "Pinny TheSeal"}`),
				wantMergedTokens: &oidctypes.Token{
					AccessToken: &oidctypes.AccessToken{
						Token:  "test-access-token",
						Type:   "test-token-type",
						Expiry: metav1.NewTime(expiryTime),
					},
					RefreshToken: &oidctypes.RefreshToken{
						Token: "test-initial-refresh-token",
					},
					IDToken: &oidctypes.IDToken{
						Token: goodIDToken,
						Claims: map[string]interface{}{
							"iss":   "some-issuer",
							"nonce": "some-nonce",
							"sub":   "some-subject",
							"name":  "Pinny TheSeal",
						},
					},
				},
			},
			{
				name:           "claims from userinfo override id token claims",
				tok:            testTokenWithoutIDToken.WithExtra(map[string]interface{}{"id_token": "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJzb21lLXN1YmplY3QiLCJuYW1lIjoiSm9obiBEb2UiLCJpc3MiOiJzb21lLWlzc3VlciIsIm5vbmNlIjoic29tZS1ub25jZSJ9.sBWi3_4cfGwrmMFZWkCghw4uvCnHN35h9xNX1gkwOtj6Oz_yKqpj7wfO4AqeWsRyrDGnkmIZbVuhAAJqPSi4GlNzN4NU8zh53PGDUpFlpDI1dvqDjIRb9iIEJpRIj34--Sz41H0ooxviIzvUdZFvQlaSzLOqgjR3ddHe2urhbtUuz_DsabP84AWo2DSg0y3ull6DRvk_DvzC6HNN8JwVi08fFvvV9BVq8kjdVeob7gajJkuGSTjsxNZGs5rbBuxBx0MZTQ8boR1fDNdG70GoIb4SsCoBSs7pZxtmGZPHInteY1SilHDDDmpQuE-LvSmvvPN_Cyk1d3eS-IR7hBbCAA"}),
				nonce:          "some-nonce",
				requireIDToken: true,
				rawClaims:      []byte(`{"userinfo_endpoint": "not-empty"}`),
				userInfo:       forceUserInfoWithClaims("some-subject", `{"name": "Pinny TheSeal"}`),
				wantMergedTokens: &oidctypes.Token{
					AccessToken: &oidctypes.AccessToken{
						Token:  "test-access-token",
						Type:   "test-token-type",
						Expiry: metav1.NewTime(expiryTime),
					},
					RefreshToken: &oidctypes.RefreshToken{
						Token: "test-initial-refresh-token",
					},
					IDToken: &oidctypes.IDToken{
						Token: "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJzb21lLXN1YmplY3QiLCJuYW1lIjoiSm9obiBEb2UiLCJpc3MiOiJzb21lLWlzc3VlciIsIm5vbmNlIjoic29tZS1ub25jZSJ9.sBWi3_4cfGwrmMFZWkCghw4uvCnHN35h9xNX1gkwOtj6Oz_yKqpj7wfO4AqeWsRyrDGnkmIZbVuhAAJqPSi4GlNzN4NU8zh53PGDUpFlpDI1dvqDjIRb9iIEJpRIj34--Sz41H0ooxviIzvUdZFvQlaSzLOqgjR3ddHe2urhbtUuz_DsabP84AWo2DSg0y3ull6DRvk_DvzC6HNN8JwVi08fFvvV9BVq8kjdVeob7gajJkuGSTjsxNZGs5rbBuxBx0MZTQ8boR1fDNdG70GoIb4SsCoBSs7pZxtmGZPHInteY1SilHDDDmpQuE-LvSmvvPN_Cyk1d3eS-IR7hBbCAA",
						Claims: map[string]interface{}{
							"iss":   "some-issuer", // takes the issuer from the ID token, since the userinfo one is unreliable.
							"nonce": "some-nonce",
							"sub":   "some-subject",
							"name":  "Pinny TheSeal",
						},
					},
				},
			},
			{
				name:           "token with id, access and refresh tokens and valid nonce, but userinfo has a different issuer",
				tok:            testTokenWithoutIDToken.WithExtra(map[string]interface{}{"id_token": goodIDToken}),
				nonce:          "some-nonce",
				requireIDToken: true,
				rawClaims:      []byte(`{"userinfo_endpoint": "not-empty"}`),
				userInfo:       forceUserInfoWithClaims("some-subject", `{"name": "Pinny TheSeal", "iss": "some-other-issuer"}`),
				wantMergedTokens: &oidctypes.Token{
					AccessToken: &oidctypes.AccessToken{
						Token:  "test-access-token",
						Type:   "test-token-type",
						Expiry: metav1.NewTime(expiryTime),
					},
					RefreshToken: &oidctypes.RefreshToken{
						Token: "test-initial-refresh-token",
					},
					IDToken: &oidctypes.IDToken{
						Token: goodIDToken,
						Claims: map[string]interface{}{
							"iss":   "some-issuer", // takes the issuer from the ID token, since the userinfo one is unreliable.
							"nonce": "some-nonce",
							"sub":   "some-subject",
							"name":  "Pinny TheSeal",
						},
					},
				},
			},
			{
				name:           "token with no id token but valid userinfo",
				tok:            testTokenWithoutIDToken,
				nonce:          "",
				requireIDToken: false,
				rawClaims:      []byte(`{"userinfo_endpoint": "not-empty"}`),
				userInfo:       forceUserInfoWithClaims("some-subject", `{"name": "Pinny TheSeal", "iss": "some-other-issuer"}`),
				wantMergedTokens: &oidctypes.Token{
					AccessToken: &oidctypes.AccessToken{
						Token:  "test-access-token",
						Type:   "test-token-type",
						Expiry: metav1.NewTime(expiryTime),
					},
					RefreshToken: &oidctypes.RefreshToken{
						Token: "test-initial-refresh-token",
					},
					IDToken: &oidctypes.IDToken{
						Token: "",
						Claims: map[string]interface{}{
							"sub":  "some-subject",
							"name": "Pinny TheSeal",
						},
					},
				},
			},
			{
				name:           "token with neither id token nor userinfo",
				tok:            testTokenWithoutIDToken,
				nonce:          "",
				requireIDToken: false,
				rawClaims:      []byte(`{"userinfo_endpoint": "not-empty"}`),
				wantMergedTokens: &oidctypes.Token{
					AccessToken: &oidctypes.AccessToken{
						Token:  "test-access-token",
						Type:   "test-token-type",
						Expiry: metav1.NewTime(expiryTime),
					},
					RefreshToken: &oidctypes.RefreshToken{
						Token: "test-initial-refresh-token",
					},
					IDToken: &oidctypes.IDToken{
						Claims: map[string]interface{}{},
					},
				},
			},
			{
				name:           "token with id, access and refresh tokens, valid nonce, and userinfo subject that doesn't match",
				tok:            testTokenWithoutIDToken.WithExtra(map[string]interface{}{"id_token": goodIDToken}),
				nonce:          "some-nonce",
				requireIDToken: true,
				rawClaims:      []byte(`{"userinfo_endpoint": "not-empty"}`),
				userInfo:       forceUserInfoWithClaims("some-other-subject", `{"name": "Pinny TheSeal"}`),
				wantErr:        "could not fetch user info claims: userinfo 'sub' claim (some-other-subject) did not match id_token 'sub' claim (some-subject)",
			},
			{
				name:           "id token not required but is provided, and subjects don't match",
				tok:            testTokenWithoutIDToken.WithExtra(map[string]interface{}{"id_token": goodIDToken}),
				nonce:          "some-nonce",
				requireIDToken: false,
				rawClaims:      []byte(`{"userinfo_endpoint": "not-empty"}`),
				userInfo:       forceUserInfoWithClaims("some-other-subject", `{"name": "Pinny TheSeal"}`),
				wantErr:        "could not fetch user info claims: userinfo 'sub' claim (some-other-subject) did not match id_token 'sub' claim (some-subject)",
			},
			{
				name:           "invalid id token",
				tok:            testTokenWithoutIDToken.WithExtra(map[string]interface{}{"id_token": "not-an-id-token"}),
				nonce:          "some-nonce",
				requireIDToken: true,
				rawClaims:      []byte(`{"userinfo_endpoint": "not-empty"}`),
				userInfo:       forceUserInfoWithClaims("some-other-subject", `{"name": "Pinny TheSeal"}`),
				wantErr:        "received invalid ID token: oidc: malformed jwt: square/go-jose: compact JWS format must have three parts",
			},
			{
				name:           "invalid nonce",
				tok:            testTokenWithoutIDToken.WithExtra(map[string]interface{}{"id_token": goodIDToken}),
				nonce:          "some-other-nonce",
				requireIDToken: true,
				rawClaims:      []byte(`{"userinfo_endpoint": "not-empty"}`),
				userInfo:       forceUserInfoWithClaims("some-other-subject", `{"name": "Pinny TheSeal"}`),
				wantErr:        "received ID token with invalid nonce: invalid nonce (expected \"some-other-nonce\", got \"some-nonce\")",
			},
			{
				name:           "expected to have id token, but doesn't",
				tok:            testTokenWithoutIDToken,
				nonce:          "some-other-nonce",
				requireIDToken: true,
				rawClaims:      []byte(`{"userinfo_endpoint": "not-empty"}`),
				userInfo:       forceUserInfoWithClaims("some-other-subject", `{"name": "Pinny TheSeal"}`),
				wantErr:        "received response missing ID token",
			},
			{
				name:           "mismatched access token hash",
				tok:            testTokenWithoutIDToken,
				nonce:          "some-other-nonce",
				requireIDToken: true,
				rawClaims:      []byte(`{"userinfo_endpoint": "not-empty"}`),
				userInfo:       forceUserInfoWithClaims("some-other-subject", `{"name": "Pinny TheSeal"}`),
				wantErr:        "received response missing ID token",
			},
			{
				name:           "id token missing subject, skip userinfo check",
				tok:            testTokenWithoutIDToken.WithExtra(map[string]interface{}{"id_token": "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJuYW1lIjoiSm9obiBEb2UiLCJpc3MiOiJzb21lLWlzc3VlciIsIm5vbmNlIjoic29tZS1ub25jZSJ9.aIhrhikAnQ4Mb1g6RAT08qqflT2LLLi2yj4F2S4zud8nYad4tfEd2ITVJ4Njdjf70ubqyzZ6XxojtC4OqaWbDaQOcd95sd3PW58SYrf4NMvEStFkcMG0HMhJEZLVGnuJQstuq3G9h5Z5bFCkx4mFNo5ho_isBWyHpk-uF14duXXlIDB10SnyZ9dRbcmu-3mMOq0g4oCUPEDiHWkv-Rf70Mk0harL2xvcpxlSMLK4glDfiiki5gl6IReIo4rTVosXAqv3JmjLDeVLtJQRG6F8YcIlDCIfUEUfk0GeYacBVjoDIO570ywVJy1LGvyUuvgXNQUjq2JgzCfb8HWGp7iJdQ"}),
				nonce:          "some-nonce",
				requireIDToken: true,
				rawClaims:      []byte(`{"userinfo_endpoint": "not-empty"}`),
				userInfo:       forceUserInfoWithClaims("some-other-subject", `{"name": "Pinny TheSeal"}`),
				wantMergedTokens: &oidctypes.Token{
					AccessToken: &oidctypes.AccessToken{
						Token:  "test-access-token",
						Type:   "test-token-type",
						Expiry: metav1.NewTime(expiryTime),
					},
					RefreshToken: &oidctypes.RefreshToken{
						Token: "test-initial-refresh-token",
					},
					IDToken: &oidctypes.IDToken{
						Token: "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJuYW1lIjoiSm9obiBEb2UiLCJpc3MiOiJzb21lLWlzc3VlciIsIm5vbmNlIjoic29tZS1ub25jZSJ9.aIhrhikAnQ4Mb1g6RAT08qqflT2LLLi2yj4F2S4zud8nYad4tfEd2ITVJ4Njdjf70ubqyzZ6XxojtC4OqaWbDaQOcd95sd3PW58SYrf4NMvEStFkcMG0HMhJEZLVGnuJQstuq3G9h5Z5bFCkx4mFNo5ho_isBWyHpk-uF14duXXlIDB10SnyZ9dRbcmu-3mMOq0g4oCUPEDiHWkv-Rf70Mk0harL2xvcpxlSMLK4glDfiiki5gl6IReIo4rTVosXAqv3JmjLDeVLtJQRG6F8YcIlDCIfUEUfk0GeYacBVjoDIO570ywVJy1LGvyUuvgXNQUjq2JgzCfb8HWGp7iJdQ",
						Claims: map[string]interface{}{
							"iss":   "some-issuer",
							"name":  "John Doe",
							"nonce": "some-nonce",
						},
					},
				},
			},
		}

		for _, tt := range tests {
			tt := tt
			t.Run(tt.name, func(t *testing.T) {
				p := ProviderConfig{
					Name:          "test-name",
					UsernameClaim: "test-username-claim",
					GroupsClaim:   "test-groups-claim",
					Config: &oauth2.Config{
						ClientID:     "test-client-id",
						ClientSecret: "test-client-secret",
						Endpoint: oauth2.Endpoint{
							AuthURL:   "https://example.com",
							TokenURL:  "https://example.com",
							AuthStyle: oauth2.AuthStyleInParams,
						},
						Scopes: []string{"scope1", "scope2"},
					},
					Provider: &mockProvider{
						rawClaims:   tt.rawClaims,
						userInfo:    tt.userInfo,
						userInfoErr: tt.userInfoErr,
					},
				}
				gotTok, err := p.ValidateToken(context.Background(), tt.tok, tt.nonce, tt.requireIDToken)
				if tt.wantErr != "" {
					require.Error(t, err)
					require.Equal(t, tt.wantErr, err.Error())
				} else {
					require.NoError(t, err)
					require.Equal(t, tt.wantMergedTokens, gotTok)
				}
			})
		}
	})

	t.Run("ExtractUpstreamSubjectFromDownstream", func(t *testing.T) {
		tests := []struct {
			name                string
			downstreamSubject   string
			wantUpstreamSubject string
			wantErr             string
		}{
			{
				name:                "happy path",
				downstreamSubject:   "https://some-issuer?sub=some-subject",
				wantUpstreamSubject: "some-subject",
			},
			{
				name:                "subject in a subject",
				downstreamSubject:   "https://some-other-issuer?sub=https://some-issuer?sub=some-subject",
				wantUpstreamSubject: "https://some-issuer?sub=some-subject",
			},
			{
				name:              "doesn't contain sub=",
				downstreamSubject: "something-invalid",
				wantErr:           "downstream subject did not contain original upstream subject",
			},
		}
		for _, tt := range tests {
			tt := tt
			t.Run(tt.name, func(t *testing.T) {
				actualUpstreamSubject, err := ExtractUpstreamSubjectFromDownstream(tt.downstreamSubject)
				if tt.wantErr != "" {
					require.Error(t, err)
					require.Equal(t, tt.wantErr, err.Error())
				} else {
					require.NoError(t, err)
					require.Equal(t, tt.wantUpstreamSubject, actualUpstreamSubject)
				}
			})
		}

	})

	t.Run("ExchangeAuthcodeAndValidateTokens", func(t *testing.T) {
		tests := []struct {
			name        string
			authCode    string
			expectNonce nonce.Nonce
			returnIDTok string
			wantErr     string
			wantToken   oidctypes.Token

			rawClaims          []byte
			userInfo           *oidc.UserInfo
			userInfoErr        error
			wantUserInfoCalled bool
		}{
			{
				name:     "exchange fails with network error",
				authCode: "invalid-auth-code",
				wantErr:  "oauth2: cannot fetch token: 403 Forbidden\nResponse: invalid authorization code\n",
			},
			{
				name:     "missing ID token",
				authCode: "valid",
				wantErr:  "received response missing ID token",
			},
			{
				name:        "invalid ID token",
				authCode:    "valid",
				returnIDTok: "invalid-jwt",
				wantErr:     "received invalid ID token: oidc: malformed jwt: square/go-jose: compact JWS format must have three parts",
			},
			{
				name:        "invalid access token hash",
				authCode:    "valid",
				returnIDTok: invalidAccessTokenHashIDToken,
				wantErr:     "received invalid ID token: access token hash does not match value in ID token",
			},
			{
				name:        "invalid nonce",
				authCode:    "valid",
				expectNonce: "test-nonce",
				returnIDTok: invalidNonceIDToken,
				wantErr:     `received ID token with invalid nonce: invalid nonce (expected "test-nonce", got "invalid-nonce")`,
			},
			{
				name:        "invalid nonce but not checked",
				authCode:    "valid",
				expectNonce: "",
				returnIDTok: invalidNonceIDToken,
				wantToken: oidctypes.Token{
					AccessToken: &oidctypes.AccessToken{
						Token:  "test-access-token",
						Expiry: metav1.Time{},
					},
					RefreshToken: &oidctypes.RefreshToken{
						Token: "test-refresh-token",
					},
					IDToken: &oidctypes.IDToken{
						Token:  invalidNonceIDToken,
						Expiry: metav1.Time{},
						Claims: map[string]interface{}{
							"aud":   "test-client-id",
							"iat":   1.602283741e+09,
							"jti":   "test-jti",
							"nbf":   1.602283741e+09,
							"nonce": "invalid-nonce",
							"sub":   "test-user",
						},
					},
				},
				rawClaims:          []byte(`{}`), // user info not supported
				wantUserInfoCalled: false,
			},
			{
				name:        "valid",
				authCode:    "valid",
				returnIDTok: validIDToken,
				wantToken: oidctypes.Token{
					AccessToken: &oidctypes.AccessToken{
						Token:  "test-access-token",
						Expiry: metav1.Time{},
					},
					RefreshToken: &oidctypes.RefreshToken{
						Token: "test-refresh-token",
					},
					IDToken: &oidctypes.IDToken{
						Token:  validIDToken,
						Expiry: metav1.Time{},
						Claims: map[string]interface{}{
							"foo": "bar",
							"bat": "baz",
							"aud": "test-client-id",
							"iat": 1.606768593e+09,
							"jti": "test-jti",
							"nbf": 1.606768593e+09,
							"sub": "test-user",
						},
					},
				},
				rawClaims:          []byte(`{}`), // user info not supported
				wantUserInfoCalled: false,
			},
			{
				name:        "user info discovery parse error",
				authCode:    "valid",
				returnIDTok: validIDToken,
				rawClaims:   []byte(`junk`), // user info discovery fails
				wantErr:     "could not fetch user info claims: could not unmarshal discovery JSON: invalid character 'j' looking for beginning of value",
			},
			{
				name:        "user info fetch error",
				authCode:    "valid",
				returnIDTok: validIDToken,
				wantErr:     "could not fetch user info claims: could not get user info: some network error",
				userInfoErr: errors.New("some network error"),
			},
			{
				name:        "user info sub error",
				authCode:    "valid",
				returnIDTok: validIDToken,
				wantErr:     "could not fetch user info claims: userinfo 'sub' claim (test-user-2) did not match id_token 'sub' claim (test-user)",
				userInfo:    &oidc.UserInfo{Subject: "test-user-2"},
			},
			{
				name:        "user info is not json",
				authCode:    "valid",
				returnIDTok: validIDToken,
				wantErr:     "could not fetch user info claims: could not unmarshal user info claims: invalid character 'i' looking for beginning of value",
				// claims is private field so we have to use hacks to set it
				userInfo: forceUserInfoWithClaims("test-user", `invalid-json-data`),
			},
			{
				name:        "valid with user info",
				authCode:    "valid",
				returnIDTok: validIDToken,
				wantToken: oidctypes.Token{
					AccessToken: &oidctypes.AccessToken{
						Token:  "test-access-token",
						Expiry: metav1.Time{},
					},
					RefreshToken: &oidctypes.RefreshToken{
						Token: "test-refresh-token",
					},
					IDToken: &oidctypes.IDToken{
						Token:  validIDToken,
						Expiry: metav1.Time{},
						Claims: map[string]interface{}{
							"foo":    "awesomeness", // overwrite existing claim
							"bat":    "baz",
							"aud":    "test-client-id",
							"iat":    1.606768593e+09,
							"jti":    "test-jti",
							"nbf":    1.606768593e+09,
							"sub":    "test-user",
							"groups": "fancy-group", // add a new claim
						},
					},
				},
				// claims is private field so we have to use hacks to set it
				userInfo:           forceUserInfoWithClaims("test-user", `{"foo":"awesomeness","groups":"fancy-group"}`),
				wantUserInfoCalled: true,
			},
			{
				name:        "invalid sub claim",
				authCode:    "valid",
				returnIDTok: invalidSubClaim,
				wantToken: oidctypes.Token{
					AccessToken: &oidctypes.AccessToken{
						Token:  "test-access-token",
						Expiry: metav1.Time{},
					},
					RefreshToken: &oidctypes.RefreshToken{
						Token: "test-refresh-token",
					},
					IDToken: &oidctypes.IDToken{
						Token:  invalidSubClaim,
						Expiry: metav1.Time{},
						Claims: map[string]interface{}{
							"foo": "bar",
							"bat": "baz",
							"aud": "test-client-id",
							"iat": 1.61021969e+09,
							"jti": "test-jti",
							"nbf": 1.61021969e+09,
							// no sub claim
						},
					},
				},
				wantUserInfoCalled: false,
			},
		}
		for _, tt := range tests {
			tt := tt
			t.Run(tt.name, func(t *testing.T) {
				tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, http.MethodPost, r.Method)
					require.NoError(t, r.ParseForm())
					require.Len(t, r.Form, 6)
					require.Equal(t, "test-client-id", r.Form.Get("client_id"))
					require.Equal(t, "test-client-secret", r.Form.Get("client_secret"))
					require.Equal(t, "test-pkce", r.Form.Get("code_verifier"))
					require.Equal(t, "authorization_code", r.Form.Get("grant_type"))
					require.Equal(t, "https://example.com/callback", r.Form.Get("redirect_uri"))
					require.NotEmpty(t, r.Form.Get("code"))
					if r.Form.Get("code") != "valid" {
						http.Error(w, "invalid authorization code", http.StatusForbidden)
						return
					}
					var response struct {
						oauth2.Token
						IDToken string `json:"id_token,omitempty"`
					}
					response.AccessToken = "test-access-token"
					response.RefreshToken = "test-refresh-token"
					response.Expiry = time.Now().Add(time.Hour)
					response.IDToken = tt.returnIDTok
					w.Header().Set("content-type", "application/json")
					require.NoError(t, json.NewEncoder(w).Encode(&response))
				}))
				t.Cleanup(tokenServer.Close)

				rawClaims := tt.rawClaims
				if len(rawClaims) == 0 && (tt.userInfo != nil || tt.userInfoErr != nil) {
					rawClaims = []byte(`{"userinfo_endpoint": "not-empty"}`)
				}

				p := ProviderConfig{
					Name:          "test-name",
					UsernameClaim: "test-username-claim",
					GroupsClaim:   "test-groups-claim",
					Config: &oauth2.Config{
						ClientID:     "test-client-id",
						ClientSecret: "test-client-secret",
						Endpoint: oauth2.Endpoint{
							AuthURL:   "https://example.com",
							TokenURL:  tokenServer.URL,
							AuthStyle: oauth2.AuthStyleInParams,
						},
						Scopes: []string{"scope1", "scope2"},
					},
					Provider: &mockProvider{
						rawClaims:   rawClaims,
						userInfo:    tt.userInfo,
						userInfoErr: tt.userInfoErr,
					},
				}

				tok, err := p.ExchangeAuthcodeAndValidateTokens(
					context.Background(),
					tt.authCode,
					"test-pkce",
					tt.expectNonce,
					"https://example.com/callback",
				)

				if tt.wantErr != "" {
					require.EqualError(t, err, tt.wantErr)
					require.Nil(t, tok)
					return
				}
				require.NoError(t, err)
				require.Equal(t, &tt.wantToken, tok)
				require.Equal(t, tt.wantUserInfoCalled, p.Provider.(*mockProvider).called)
			})
		}
	})
}

// mockVerifier returns an *oidc.IDTokenVerifier that validates any correctly serialized JWT without doing much else.
func mockVerifier() *oidc.IDTokenVerifier {
	mockKeySet := mockkeyset.NewMockKeySet(gomock.NewController(nil))
	mockKeySet.EXPECT().VerifySignature(gomock.Any(), gomock.Any()).
		AnyTimes().
		DoAndReturn(func(ctx context.Context, jwt string) ([]byte, error) {
			jws, err := jose.ParseSigned(jwt)
			if err != nil {
				return nil, err
			}
			return jws.UnsafePayloadWithoutVerification(), nil
		})

	return oidc.NewVerifier("", mockKeySet, &oidc.Config{
		SkipIssuerCheck:   true,
		SkipExpiryCheck:   true,
		SkipClientIDCheck: true,
	})
}

type mockProvider struct {
	called      bool
	rawClaims   []byte
	userInfo    *oidc.UserInfo
	userInfoErr error
}

func (m *mockProvider) Verifier(_ *oidc.Config) *oidc.IDTokenVerifier { return mockVerifier() }

func (m *mockProvider) Claims(v interface{}) error {
	return json.Unmarshal(m.rawClaims, v)
}

func (m *mockProvider) UserInfo(_ context.Context, tokenSource oauth2.TokenSource) (*oidc.UserInfo, error) {
	m.called = true

	token, err := tokenSource.Token()
	if err != nil {
		return nil, err
	}
	if wantToken := "test-access-token"; wantToken != token.AccessToken {
		return nil, fmt.Errorf("want token = %#v, got token = %#v", wantToken, token)
	}

	return m.userInfo, m.userInfoErr
}

func forceUserInfoWithClaims(subject string, claims string) *oidc.UserInfo {
	userInfo := &oidc.UserInfo{Subject: subject}

	// this is some dark magic to set a private field
	claimsField := reflect.ValueOf(userInfo).Elem().FieldByName("claims")
	claimsPointer := (*[]byte)(unsafe.Pointer(claimsField.UnsafeAddr()))
	*claimsPointer = []byte(claims)

	return userInfo
}
