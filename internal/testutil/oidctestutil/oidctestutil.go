// Copyright 2020-2021 the Pinniped contributors. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package oidctestutil

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	coreosoidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/ory/fosite"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"gopkg.in/square/go-jose.v2"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"go.pinniped.dev/internal/authenticators"
	"go.pinniped.dev/internal/crud"
	"go.pinniped.dev/internal/fositestorage/authorizationcode"
	"go.pinniped.dev/internal/fositestorage/openidconnect"
	pkce2 "go.pinniped.dev/internal/fositestorage/pkce"
	"go.pinniped.dev/internal/fositestoragei"
	"go.pinniped.dev/internal/oidc/provider"
	"go.pinniped.dev/internal/psession"
	"go.pinniped.dev/internal/testutil"
	"go.pinniped.dev/pkg/oidcclient/nonce"
	"go.pinniped.dev/pkg/oidcclient/oidctypes"
	"go.pinniped.dev/pkg/oidcclient/pkce"
)

// Test helpers for the OIDC package.

// ExchangeAuthcodeAndValidateTokenArgs is used to spy on calls to
// TestUpstreamOIDCIdentityProvider.ExchangeAuthcodeAndValidateTokensFunc().
type ExchangeAuthcodeAndValidateTokenArgs struct {
	Ctx                  context.Context
	Authcode             string
	PKCECodeVerifier     pkce.Code
	ExpectedIDTokenNonce nonce.Nonce
	RedirectURI          string
}

// PasswordCredentialsGrantAndValidateTokensArgs is used to spy on calls to
// TestUpstreamOIDCIdentityProvider.PasswordCredentialsGrantAndValidateTokensFunc().
type PasswordCredentialsGrantAndValidateTokensArgs struct {
	Ctx      context.Context
	Username string
	Password string
}

// PerformRefreshArgs is used to spy on calls to
// TestUpstreamOIDCIdentityProvider.PerformRefreshFunc().
type PerformRefreshArgs struct {
	Ctx              context.Context
	RefreshToken     string
	DN               string
	ExpectedUsername string
	ExpectedSubject  string
}

// RevokeRefreshTokenArgs is used to spy on calls to
// TestUpstreamOIDCIdentityProvider.RevokeRefreshTokenArgsFunc().
type RevokeRefreshTokenArgs struct {
	Ctx          context.Context
	RefreshToken string
}

// ValidateTokenArgs is used to spy on calls to
// TestUpstreamOIDCIdentityProvider.ValidateTokenFunc().
type ValidateTokenArgs struct {
	Ctx                  context.Context
	Tok                  *oauth2.Token
	ExpectedIDTokenNonce nonce.Nonce
}

type ValidateRefreshArgs struct {
	Ctx              context.Context
	Tok              *oauth2.Token
	StoredAttributes provider.StoredRefreshAttributes
}

type TestUpstreamLDAPIdentityProvider struct {
	Name                    string
	ResourceUID             types.UID
	URL                     *url.URL
	AuthenticateFunc        func(ctx context.Context, username, password string) (*authenticators.Response, bool, error)
	performRefreshCallCount int
	performRefreshArgs      []*PerformRefreshArgs
	PerformRefreshErr       error
}

var _ provider.UpstreamLDAPIdentityProviderI = &TestUpstreamLDAPIdentityProvider{}

func (u *TestUpstreamLDAPIdentityProvider) GetResourceUID() types.UID {
	return u.ResourceUID
}

func (u *TestUpstreamLDAPIdentityProvider) GetName() string {
	return u.Name
}

func (u *TestUpstreamLDAPIdentityProvider) AuthenticateUser(ctx context.Context, username, password string) (*authenticators.Response, bool, error) {
	return u.AuthenticateFunc(ctx, username, password)
}

func (u *TestUpstreamLDAPIdentityProvider) GetURL() *url.URL {
	return u.URL
}

func (u *TestUpstreamLDAPIdentityProvider) PerformRefresh(ctx context.Context, storedRefreshAttributes provider.StoredRefreshAttributes) error {
	if u.performRefreshArgs == nil {
		u.performRefreshArgs = make([]*PerformRefreshArgs, 0)
	}
	u.performRefreshCallCount++
	u.performRefreshArgs = append(u.performRefreshArgs, &PerformRefreshArgs{
		Ctx:              ctx,
		DN:               storedRefreshAttributes.DN,
		ExpectedUsername: storedRefreshAttributes.Username,
		ExpectedSubject:  storedRefreshAttributes.Subject,
	})
	if u.PerformRefreshErr != nil {
		return u.PerformRefreshErr
	}
	return nil
}

func (u *TestUpstreamLDAPIdentityProvider) PerformRefreshCallCount() int {
	return u.performRefreshCallCount
}

func (u *TestUpstreamLDAPIdentityProvider) PerformRefreshArgs(call int) *PerformRefreshArgs {
	if u.performRefreshArgs == nil {
		u.performRefreshArgs = make([]*PerformRefreshArgs, 0)
	}
	return u.performRefreshArgs[call]
}

type TestUpstreamOIDCIdentityProvider struct {
	Name                     string
	ClientID                 string
	ResourceUID              types.UID
	AuthorizationURL         url.URL
	RevocationURL            *url.URL
	UsernameClaim            string
	GroupsClaim              string
	Scopes                   []string
	AdditionalAuthcodeParams map[string]string
	AllowPasswordGrant       bool

	ExchangeAuthcodeAndValidateTokensFunc func(
		ctx context.Context,
		authcode string,
		pkceCodeVerifier pkce.Code,
		expectedIDTokenNonce nonce.Nonce,
	) (*oidctypes.Token, error)

	PasswordCredentialsGrantAndValidateTokensFunc func(
		ctx context.Context,
		username string,
		password string,
	) (*oidctypes.Token, error)

	PerformRefreshFunc func(ctx context.Context, refreshToken string) (*oauth2.Token, error)

	RevokeRefreshTokenFunc func(ctx context.Context, refreshToken string) error

	ValidateTokenFunc func(ctx context.Context, tok *oauth2.Token, expectedIDTokenNonce nonce.Nonce) (*oidctypes.Token, error)

	ValidateRefreshFunc func(ctx context.Context, tok *oauth2.Token, storedAttributes provider.StoredRefreshAttributes) error

	exchangeAuthcodeAndValidateTokensCallCount         int
	exchangeAuthcodeAndValidateTokensArgs              []*ExchangeAuthcodeAndValidateTokenArgs
	passwordCredentialsGrantAndValidateTokensCallCount int
	passwordCredentialsGrantAndValidateTokensArgs      []*PasswordCredentialsGrantAndValidateTokensArgs
	performRefreshCallCount                            int
	performRefreshArgs                                 []*PerformRefreshArgs
	revokeRefreshTokenCallCount                        int
	revokeRefreshTokenArgs                             []*RevokeRefreshTokenArgs
	validateTokenCallCount                             int
	validateTokenArgs                                  []*ValidateTokenArgs
	validateRefreshCallCount                           int
	validateRefreshArgs                                []*ValidateRefreshArgs
}

var _ provider.UpstreamOIDCIdentityProviderI = &TestUpstreamOIDCIdentityProvider{}

func (u *TestUpstreamOIDCIdentityProvider) GetResourceUID() types.UID {
	return u.ResourceUID
}

func (u *TestUpstreamOIDCIdentityProvider) GetAdditionalAuthcodeParams() map[string]string {
	return u.AdditionalAuthcodeParams
}

func (u *TestUpstreamOIDCIdentityProvider) GetName() string {
	return u.Name
}

func (u *TestUpstreamOIDCIdentityProvider) GetClientID() string {
	return u.ClientID
}

func (u *TestUpstreamOIDCIdentityProvider) GetAuthorizationURL() *url.URL {
	return &u.AuthorizationURL
}

func (u *TestUpstreamOIDCIdentityProvider) GetRevocationURL() *url.URL {
	return u.RevocationURL
}

func (u *TestUpstreamOIDCIdentityProvider) GetScopes() []string {
	return u.Scopes
}

func (u *TestUpstreamOIDCIdentityProvider) GetUsernameClaim() string {
	return u.UsernameClaim
}

func (u *TestUpstreamOIDCIdentityProvider) GetGroupsClaim() string {
	return u.GroupsClaim
}

func (u *TestUpstreamOIDCIdentityProvider) AllowsPasswordGrant() bool {
	return u.AllowPasswordGrant
}

func (u *TestUpstreamOIDCIdentityProvider) PasswordCredentialsGrantAndValidateTokens(ctx context.Context, username, password string) (*oidctypes.Token, error) {
	u.passwordCredentialsGrantAndValidateTokensCallCount++
	u.passwordCredentialsGrantAndValidateTokensArgs = append(u.passwordCredentialsGrantAndValidateTokensArgs, &PasswordCredentialsGrantAndValidateTokensArgs{
		Ctx:      ctx,
		Username: username,
		Password: password,
	})
	return u.PasswordCredentialsGrantAndValidateTokensFunc(ctx, username, password)
}

func (u *TestUpstreamOIDCIdentityProvider) ExchangeAuthcodeAndValidateTokens(
	ctx context.Context,
	authcode string,
	pkceCodeVerifier pkce.Code,
	expectedIDTokenNonce nonce.Nonce,
	redirectURI string,
) (*oidctypes.Token, error) {
	if u.exchangeAuthcodeAndValidateTokensArgs == nil {
		u.exchangeAuthcodeAndValidateTokensArgs = make([]*ExchangeAuthcodeAndValidateTokenArgs, 0)
	}
	u.exchangeAuthcodeAndValidateTokensCallCount++
	u.exchangeAuthcodeAndValidateTokensArgs = append(u.exchangeAuthcodeAndValidateTokensArgs, &ExchangeAuthcodeAndValidateTokenArgs{
		Ctx:                  ctx,
		Authcode:             authcode,
		PKCECodeVerifier:     pkceCodeVerifier,
		ExpectedIDTokenNonce: expectedIDTokenNonce,
		RedirectURI:          redirectURI,
	})
	return u.ExchangeAuthcodeAndValidateTokensFunc(ctx, authcode, pkceCodeVerifier, expectedIDTokenNonce)
}

func (u *TestUpstreamOIDCIdentityProvider) ExchangeAuthcodeAndValidateTokensCallCount() int {
	return u.exchangeAuthcodeAndValidateTokensCallCount
}

func (u *TestUpstreamOIDCIdentityProvider) ExchangeAuthcodeAndValidateTokensArgs(call int) *ExchangeAuthcodeAndValidateTokenArgs {
	if u.exchangeAuthcodeAndValidateTokensArgs == nil {
		u.exchangeAuthcodeAndValidateTokensArgs = make([]*ExchangeAuthcodeAndValidateTokenArgs, 0)
	}
	return u.exchangeAuthcodeAndValidateTokensArgs[call]
}

func (u *TestUpstreamOIDCIdentityProvider) PerformRefresh(ctx context.Context, refreshToken string) (*oauth2.Token, error) {
	if u.performRefreshArgs == nil {
		u.performRefreshArgs = make([]*PerformRefreshArgs, 0)
	}
	u.performRefreshCallCount++
	u.performRefreshArgs = append(u.performRefreshArgs, &PerformRefreshArgs{
		Ctx:          ctx,
		RefreshToken: refreshToken,
	})
	return u.PerformRefreshFunc(ctx, refreshToken)
}

func (u *TestUpstreamOIDCIdentityProvider) ValidateRefresh(ctx context.Context, tok *oauth2.Token, storedAttributes provider.StoredRefreshAttributes) error {
	if u.validateRefreshArgs == nil {
		u.validateRefreshArgs = make([]*ValidateRefreshArgs, 0)
	}
	u.validateRefreshCallCount++
	u.validateRefreshArgs = append(u.validateRefreshArgs, &ValidateRefreshArgs{
		Ctx:              ctx,
		Tok:              tok,
		StoredAttributes: storedAttributes,
	})
	return u.ValidateRefreshFunc(ctx, tok, storedAttributes)
}

func (u *TestUpstreamOIDCIdentityProvider) RevokeRefreshToken(ctx context.Context, refreshToken string) error {
	if u.revokeRefreshTokenArgs == nil {
		u.revokeRefreshTokenArgs = make([]*RevokeRefreshTokenArgs, 0)
	}
	u.revokeRefreshTokenCallCount++
	u.revokeRefreshTokenArgs = append(u.revokeRefreshTokenArgs, &RevokeRefreshTokenArgs{
		Ctx:          ctx,
		RefreshToken: refreshToken,
	})
	return u.RevokeRefreshTokenFunc(ctx, refreshToken)
}

func (u *TestUpstreamOIDCIdentityProvider) PerformRefreshCallCount() int {
	return u.performRefreshCallCount
}

func (u *TestUpstreamOIDCIdentityProvider) PerformRefreshArgs(call int) *PerformRefreshArgs {
	if u.performRefreshArgs == nil {
		u.performRefreshArgs = make([]*PerformRefreshArgs, 0)
	}
	return u.performRefreshArgs[call]
}

func (u *TestUpstreamOIDCIdentityProvider) RevokeRefreshTokenCallCount() int {
	return u.performRefreshCallCount
}

func (u *TestUpstreamOIDCIdentityProvider) RevokeRefreshTokenArgs(call int) *RevokeRefreshTokenArgs {
	if u.revokeRefreshTokenArgs == nil {
		u.revokeRefreshTokenArgs = make([]*RevokeRefreshTokenArgs, 0)
	}
	return u.revokeRefreshTokenArgs[call]
}

func (u *TestUpstreamOIDCIdentityProvider) ValidateToken(ctx context.Context, tok *oauth2.Token, expectedIDTokenNonce nonce.Nonce, requireIDToken bool) (*oidctypes.Token, error) {
	if u.validateTokenArgs == nil {
		u.validateTokenArgs = make([]*ValidateTokenArgs, 0)
	}
	u.validateTokenCallCount++
	u.validateTokenArgs = append(u.validateTokenArgs, &ValidateTokenArgs{
		Ctx:                  ctx,
		Tok:                  tok,
		ExpectedIDTokenNonce: expectedIDTokenNonce,
	})
	return u.ValidateTokenFunc(ctx, tok, expectedIDTokenNonce)
}

func (u *TestUpstreamOIDCIdentityProvider) ValidateTokenCallCount() int {
	return u.validateTokenCallCount
}

func (u *TestUpstreamOIDCIdentityProvider) ValidateTokenArgs(call int) *ValidateTokenArgs {
	if u.validateTokenArgs == nil {
		u.validateTokenArgs = make([]*ValidateTokenArgs, 0)
	}
	return u.validateTokenArgs[call]
}

type UpstreamIDPListerBuilder struct {
	upstreamOIDCIdentityProviders            []*TestUpstreamOIDCIdentityProvider
	upstreamLDAPIdentityProviders            []*TestUpstreamLDAPIdentityProvider
	upstreamActiveDirectoryIdentityProviders []*TestUpstreamLDAPIdentityProvider
}

func (b *UpstreamIDPListerBuilder) WithOIDC(upstreamOIDCIdentityProviders ...*TestUpstreamOIDCIdentityProvider) *UpstreamIDPListerBuilder {
	b.upstreamOIDCIdentityProviders = append(b.upstreamOIDCIdentityProviders, upstreamOIDCIdentityProviders...)
	return b
}

func (b *UpstreamIDPListerBuilder) WithLDAP(upstreamLDAPIdentityProviders ...*TestUpstreamLDAPIdentityProvider) *UpstreamIDPListerBuilder {
	b.upstreamLDAPIdentityProviders = append(b.upstreamLDAPIdentityProviders, upstreamLDAPIdentityProviders...)
	return b
}

func (b *UpstreamIDPListerBuilder) WithActiveDirectory(upstreamActiveDirectoryIdentityProviders ...*TestUpstreamLDAPIdentityProvider) *UpstreamIDPListerBuilder {
	b.upstreamActiveDirectoryIdentityProviders = append(b.upstreamActiveDirectoryIdentityProviders, upstreamActiveDirectoryIdentityProviders...)
	return b
}

func (b *UpstreamIDPListerBuilder) Build() provider.DynamicUpstreamIDPProvider {
	idpProvider := provider.NewDynamicUpstreamIDPProvider()

	oidcUpstreams := make([]provider.UpstreamOIDCIdentityProviderI, len(b.upstreamOIDCIdentityProviders))
	for i := range b.upstreamOIDCIdentityProviders {
		oidcUpstreams[i] = provider.UpstreamOIDCIdentityProviderI(b.upstreamOIDCIdentityProviders[i])
	}
	idpProvider.SetOIDCIdentityProviders(oidcUpstreams)

	ldapUpstreams := make([]provider.UpstreamLDAPIdentityProviderI, len(b.upstreamLDAPIdentityProviders))
	for i := range b.upstreamLDAPIdentityProviders {
		ldapUpstreams[i] = provider.UpstreamLDAPIdentityProviderI(b.upstreamLDAPIdentityProviders[i])
	}
	idpProvider.SetLDAPIdentityProviders(ldapUpstreams)

	adUpstreams := make([]provider.UpstreamLDAPIdentityProviderI, len(b.upstreamActiveDirectoryIdentityProviders))
	for i := range b.upstreamActiveDirectoryIdentityProviders {
		adUpstreams[i] = provider.UpstreamLDAPIdentityProviderI(b.upstreamActiveDirectoryIdentityProviders[i])
	}
	idpProvider.SetActiveDirectoryIdentityProviders(adUpstreams)

	return idpProvider
}

func (b *UpstreamIDPListerBuilder) RequireExactlyOneCallToPasswordCredentialsGrantAndValidateTokens(
	t *testing.T,
	expectedPerformedByUpstreamName string,
	expectedArgs *PasswordCredentialsGrantAndValidateTokensArgs,
) {
	t.Helper()
	var actualArgs *PasswordCredentialsGrantAndValidateTokensArgs
	var actualNameOfUpstreamWhichMadeCall string
	actualCallCountAcrossAllOIDCUpstreams := 0
	for _, upstreamOIDC := range b.upstreamOIDCIdentityProviders {
		callCountOnThisUpstream := upstreamOIDC.passwordCredentialsGrantAndValidateTokensCallCount
		actualCallCountAcrossAllOIDCUpstreams += callCountOnThisUpstream
		if callCountOnThisUpstream == 1 {
			actualNameOfUpstreamWhichMadeCall = upstreamOIDC.Name
			actualArgs = upstreamOIDC.passwordCredentialsGrantAndValidateTokensArgs[0]
		}
	}
	require.Equal(t, 1, actualCallCountAcrossAllOIDCUpstreams,
		"should have been exactly one call to PasswordCredentialsGrantAndValidateTokens() by all OIDC upstreams",
	)
	require.Equal(t, expectedPerformedByUpstreamName, actualNameOfUpstreamWhichMadeCall,
		"PasswordCredentialsGrantAndValidateTokens() was called on the wrong OIDC upstream",
	)
	require.Equal(t, expectedArgs, actualArgs)
}

func (b *UpstreamIDPListerBuilder) RequireExactlyZeroCallsToPasswordCredentialsGrantAndValidateTokens(t *testing.T) {
	t.Helper()
	actualCallCountAcrossAllOIDCUpstreams := 0
	for _, upstreamOIDC := range b.upstreamOIDCIdentityProviders {
		actualCallCountAcrossAllOIDCUpstreams += upstreamOIDC.passwordCredentialsGrantAndValidateTokensCallCount
	}
	require.Equal(t, 0, actualCallCountAcrossAllOIDCUpstreams,
		"expected exactly zero calls to PasswordCredentialsGrantAndValidateTokens()",
	)
}

func (b *UpstreamIDPListerBuilder) RequireExactlyOneCallToExchangeAuthcodeAndValidateTokens(
	t *testing.T,
	expectedPerformedByUpstreamName string,
	expectedArgs *ExchangeAuthcodeAndValidateTokenArgs,
) {
	t.Helper()
	var actualArgs *ExchangeAuthcodeAndValidateTokenArgs
	var actualNameOfUpstreamWhichMadeCall string
	actualCallCountAcrossAllOIDCUpstreams := 0
	for _, upstreamOIDC := range b.upstreamOIDCIdentityProviders {
		callCountOnThisUpstream := upstreamOIDC.exchangeAuthcodeAndValidateTokensCallCount
		actualCallCountAcrossAllOIDCUpstreams += callCountOnThisUpstream
		if callCountOnThisUpstream == 1 {
			actualNameOfUpstreamWhichMadeCall = upstreamOIDC.Name
			actualArgs = upstreamOIDC.exchangeAuthcodeAndValidateTokensArgs[0]
		}
	}
	require.Equal(t, 1, actualCallCountAcrossAllOIDCUpstreams,
		"should have been exactly one call to ExchangeAuthcodeAndValidateTokens() by all OIDC upstreams",
	)
	require.Equal(t, expectedPerformedByUpstreamName, actualNameOfUpstreamWhichMadeCall,
		"ExchangeAuthcodeAndValidateTokens() was called on the wrong OIDC upstream",
	)
	require.Equal(t, expectedArgs, actualArgs)
}

func (b *UpstreamIDPListerBuilder) RequireExactlyZeroCallsToExchangeAuthcodeAndValidateTokens(t *testing.T) {
	t.Helper()
	actualCallCountAcrossAllOIDCUpstreams := 0
	for _, upstreamOIDC := range b.upstreamOIDCIdentityProviders {
		actualCallCountAcrossAllOIDCUpstreams += upstreamOIDC.exchangeAuthcodeAndValidateTokensCallCount
	}
	require.Equal(t, 0, actualCallCountAcrossAllOIDCUpstreams,
		"expected exactly zero calls to ExchangeAuthcodeAndValidateTokens()",
	)
}

func (b *UpstreamIDPListerBuilder) RequireExactlyOneCallToPerformRefresh(
	t *testing.T,
	expectedPerformedByUpstreamName string,
	expectedArgs *PerformRefreshArgs,
) {
	t.Helper()
	var actualArgs *PerformRefreshArgs
	var actualNameOfUpstreamWhichMadeCall string
	actualCallCountAcrossAllUpstreams := 0
	for _, upstreamOIDC := range b.upstreamOIDCIdentityProviders {
		callCountOnThisUpstream := upstreamOIDC.performRefreshCallCount
		actualCallCountAcrossAllUpstreams += callCountOnThisUpstream
		if callCountOnThisUpstream == 1 {
			actualNameOfUpstreamWhichMadeCall = upstreamOIDC.Name
			actualArgs = upstreamOIDC.performRefreshArgs[0]
		}
	}
	for _, upstreamLDAP := range b.upstreamLDAPIdentityProviders {
		callCountOnThisUpstream := upstreamLDAP.performRefreshCallCount
		actualCallCountAcrossAllUpstreams += callCountOnThisUpstream
		if callCountOnThisUpstream == 1 {
			actualNameOfUpstreamWhichMadeCall = upstreamLDAP.Name
			actualArgs = upstreamLDAP.performRefreshArgs[0]
		}
	}
	for _, upstreamAD := range b.upstreamActiveDirectoryIdentityProviders {
		callCountOnThisUpstream := upstreamAD.performRefreshCallCount
		actualCallCountAcrossAllUpstreams += callCountOnThisUpstream
		if callCountOnThisUpstream == 1 {
			actualNameOfUpstreamWhichMadeCall = upstreamAD.Name
			actualArgs = upstreamAD.performRefreshArgs[0]
		}
	}
	require.Equal(t, 1, actualCallCountAcrossAllUpstreams,
		"should have been exactly one call to PerformRefresh() by all upstreams",
	)
	require.Equal(t, expectedPerformedByUpstreamName, actualNameOfUpstreamWhichMadeCall,
		"PerformRefresh() was called on the wrong upstream",
	)
	require.Equal(t, expectedArgs, actualArgs)
}

func (b *UpstreamIDPListerBuilder) RequireExactlyZeroCallsToPerformRefresh(t *testing.T) {
	t.Helper()
	actualCallCountAcrossAllUpstreams := 0
	for _, upstreamOIDC := range b.upstreamOIDCIdentityProviders {
		actualCallCountAcrossAllUpstreams += upstreamOIDC.performRefreshCallCount
	}
	for _, upstreamLDAP := range b.upstreamLDAPIdentityProviders {
		actualCallCountAcrossAllUpstreams += upstreamLDAP.performRefreshCallCount
	}
	for _, upstreamActiveDirectory := range b.upstreamActiveDirectoryIdentityProviders {
		actualCallCountAcrossAllUpstreams += upstreamActiveDirectory.performRefreshCallCount
	}

	require.Equal(t, 0, actualCallCountAcrossAllUpstreams,
		"expected exactly zero calls to PerformRefresh()",
	)
}

func (b *UpstreamIDPListerBuilder) RequireExactlyOneCallToValidateToken(
	t *testing.T,
	expectedPerformedByUpstreamName string,
	expectedArgs *ValidateTokenArgs,
) {
	t.Helper()
	var actualArgs *ValidateTokenArgs
	var actualNameOfUpstreamWhichMadeCall string
	actualCallCountAcrossAllOIDCUpstreams := 0
	for _, upstreamOIDC := range b.upstreamOIDCIdentityProviders {
		callCountOnThisUpstream := upstreamOIDC.validateTokenCallCount
		actualCallCountAcrossAllOIDCUpstreams += callCountOnThisUpstream
		if callCountOnThisUpstream == 1 {
			actualNameOfUpstreamWhichMadeCall = upstreamOIDC.Name
			actualArgs = upstreamOIDC.validateTokenArgs[0]
		}
	}
	require.Equal(t, 1, actualCallCountAcrossAllOIDCUpstreams,
		"should have been exactly one call to ValidateToken() by all OIDC upstreams",
	)
	require.Equal(t, expectedPerformedByUpstreamName, actualNameOfUpstreamWhichMadeCall,
		"ValidateToken() was called on the wrong OIDC upstream",
	)
	require.Equal(t, expectedArgs, actualArgs)
}

func (b *UpstreamIDPListerBuilder) RequireExactlyZeroCallsToValidateToken(t *testing.T) {
	t.Helper()
	actualCallCountAcrossAllOIDCUpstreams := 0
	for _, upstreamOIDC := range b.upstreamOIDCIdentityProviders {
		actualCallCountAcrossAllOIDCUpstreams += upstreamOIDC.validateTokenCallCount
	}
	require.Equal(t, 0, actualCallCountAcrossAllOIDCUpstreams,
		"expected exactly zero calls to ValidateToken()",
	)
}

func (b *UpstreamIDPListerBuilder) RequireExactlyOneCallToRevokeRefreshToken(
	t *testing.T,
	expectedPerformedByUpstreamName string,
	expectedArgs *RevokeRefreshTokenArgs,
) {
	t.Helper()
	var actualArgs *RevokeRefreshTokenArgs
	var actualNameOfUpstreamWhichMadeCall string
	actualCallCountAcrossAllOIDCUpstreams := 0
	for _, upstreamOIDC := range b.upstreamOIDCIdentityProviders {
		callCountOnThisUpstream := upstreamOIDC.revokeRefreshTokenCallCount
		actualCallCountAcrossAllOIDCUpstreams += callCountOnThisUpstream
		if callCountOnThisUpstream == 1 {
			actualNameOfUpstreamWhichMadeCall = upstreamOIDC.Name
			actualArgs = upstreamOIDC.revokeRefreshTokenArgs[0]
		}
	}
	require.Equal(t, 1, actualCallCountAcrossAllOIDCUpstreams,
		"should have been exactly one call to RevokeRefreshToken() by all OIDC upstreams",
	)
	require.Equal(t, expectedPerformedByUpstreamName, actualNameOfUpstreamWhichMadeCall,
		"RevokeRefreshToken() was called on the wrong OIDC upstream",
	)
	require.Equal(t, expectedArgs, actualArgs)
}

func (b *UpstreamIDPListerBuilder) RequireExactlyZeroCallsToRevokeRefreshToken(t *testing.T) {
	t.Helper()
	actualCallCountAcrossAllOIDCUpstreams := 0
	for _, upstreamOIDC := range b.upstreamOIDCIdentityProviders {
		actualCallCountAcrossAllOIDCUpstreams += upstreamOIDC.revokeRefreshTokenCallCount
	}
	require.Equal(t, 0, actualCallCountAcrossAllOIDCUpstreams,
		"expected exactly zero calls to RevokeRefreshToken()",
	)
}

func NewUpstreamIDPListerBuilder() *UpstreamIDPListerBuilder {
	return &UpstreamIDPListerBuilder{}
}

type TestUpstreamOIDCIdentityProviderBuilder struct {
	name                     string
	resourceUID              types.UID
	clientID                 string
	scopes                   []string
	idToken                  map[string]interface{}
	refreshToken             *oidctypes.RefreshToken
	usernameClaim            string
	groupsClaim              string
	refreshedTokens          *oauth2.Token
	validatedTokens          *oidctypes.Token
	authorizationURL         url.URL
	additionalAuthcodeParams map[string]string
	allowPasswordGrant       bool
	authcodeExchangeErr      error
	passwordGrantErr         error
	performRefreshErr        error
	revokeRefreshTokenErr    error
	validateTokenErr         error
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithName(value string) *TestUpstreamOIDCIdentityProviderBuilder {
	u.name = value
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithResourceUID(value types.UID) *TestUpstreamOIDCIdentityProviderBuilder {
	u.resourceUID = value
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithClientID(value string) *TestUpstreamOIDCIdentityProviderBuilder {
	u.clientID = value
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithAuthorizationURL(value url.URL) *TestUpstreamOIDCIdentityProviderBuilder {
	u.authorizationURL = value
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithAllowPasswordGrant(value bool) *TestUpstreamOIDCIdentityProviderBuilder {
	u.allowPasswordGrant = value
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithScopes(values []string) *TestUpstreamOIDCIdentityProviderBuilder {
	u.scopes = values
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithUsernameClaim(value string) *TestUpstreamOIDCIdentityProviderBuilder {
	u.usernameClaim = value
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithoutUsernameClaim() *TestUpstreamOIDCIdentityProviderBuilder {
	u.usernameClaim = ""
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithGroupsClaim(value string) *TestUpstreamOIDCIdentityProviderBuilder {
	u.groupsClaim = value
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithoutGroupsClaim() *TestUpstreamOIDCIdentityProviderBuilder {
	u.groupsClaim = ""
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithIDTokenClaim(name string, value interface{}) *TestUpstreamOIDCIdentityProviderBuilder {
	if u.idToken == nil {
		u.idToken = map[string]interface{}{}
	}
	u.idToken[name] = value
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithoutIDTokenClaim(claim string) *TestUpstreamOIDCIdentityProviderBuilder {
	delete(u.idToken, claim)
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithAdditionalAuthcodeParams(params map[string]string) *TestUpstreamOIDCIdentityProviderBuilder {
	u.additionalAuthcodeParams = params
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithRefreshToken(token string) *TestUpstreamOIDCIdentityProviderBuilder {
	u.refreshToken = &oidctypes.RefreshToken{Token: token}
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithEmptyRefreshToken() *TestUpstreamOIDCIdentityProviderBuilder {
	u.refreshToken = &oidctypes.RefreshToken{Token: ""}
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithoutRefreshToken() *TestUpstreamOIDCIdentityProviderBuilder {
	u.refreshToken = nil
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithUpstreamAuthcodeExchangeError(err error) *TestUpstreamOIDCIdentityProviderBuilder {
	u.authcodeExchangeErr = err
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithPasswordGrantError(err error) *TestUpstreamOIDCIdentityProviderBuilder {
	u.passwordGrantErr = err
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithRefreshedTokens(tokens *oauth2.Token) *TestUpstreamOIDCIdentityProviderBuilder {
	u.refreshedTokens = tokens
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithPerformRefreshError(err error) *TestUpstreamOIDCIdentityProviderBuilder {
	u.performRefreshErr = err
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithValidatedTokens(tokens *oidctypes.Token) *TestUpstreamOIDCIdentityProviderBuilder {
	u.validatedTokens = tokens
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithValidateTokenError(err error) *TestUpstreamOIDCIdentityProviderBuilder {
	u.validateTokenErr = err
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) WithRevokeRefreshTokenError(err error) *TestUpstreamOIDCIdentityProviderBuilder {
	u.revokeRefreshTokenErr = err
	return u
}

func (u *TestUpstreamOIDCIdentityProviderBuilder) Build() *TestUpstreamOIDCIdentityProvider {
	return &TestUpstreamOIDCIdentityProvider{
		Name:                     u.name,
		ClientID:                 u.clientID,
		ResourceUID:              u.resourceUID,
		UsernameClaim:            u.usernameClaim,
		GroupsClaim:              u.groupsClaim,
		Scopes:                   u.scopes,
		AllowPasswordGrant:       u.allowPasswordGrant,
		AuthorizationURL:         u.authorizationURL,
		AdditionalAuthcodeParams: u.additionalAuthcodeParams,
		ExchangeAuthcodeAndValidateTokensFunc: func(ctx context.Context, authcode string, pkceCodeVerifier pkce.Code, expectedIDTokenNonce nonce.Nonce) (*oidctypes.Token, error) {
			if u.authcodeExchangeErr != nil {
				return nil, u.authcodeExchangeErr
			}
			return &oidctypes.Token{IDToken: &oidctypes.IDToken{Claims: u.idToken}, RefreshToken: u.refreshToken}, nil
		},
		PasswordCredentialsGrantAndValidateTokensFunc: func(ctx context.Context, username, password string) (*oidctypes.Token, error) {
			if u.passwordGrantErr != nil {
				return nil, u.passwordGrantErr
			}
			return &oidctypes.Token{IDToken: &oidctypes.IDToken{Claims: u.idToken}, RefreshToken: u.refreshToken}, nil
		},
		PerformRefreshFunc: func(ctx context.Context, refreshToken string) (*oauth2.Token, error) {
			if u.performRefreshErr != nil {
				return nil, u.performRefreshErr
			}
			return u.refreshedTokens, nil
		},
		RevokeRefreshTokenFunc: func(ctx context.Context, refreshToken string) error {
			return u.revokeRefreshTokenErr
		},
		ValidateTokenFunc: func(ctx context.Context, tok *oauth2.Token, expectedIDTokenNonce nonce.Nonce) (*oidctypes.Token, error) {
			if u.validateTokenErr != nil {
				return nil, u.validateTokenErr
			}
			return u.validatedTokens, nil
		},
	}
}

func NewTestUpstreamOIDCIdentityProviderBuilder() *TestUpstreamOIDCIdentityProviderBuilder {
	return &TestUpstreamOIDCIdentityProviderBuilder{}
}

// Declare a separate type from the production code to ensure that the state param's contents was serialized
// in the format that we expect, with the json keys that we expect, etc. This also ensure that the order of
// the serialized fields is the same, which doesn't really matter expect that we can make simpler equality
// assertions about the redirect URL in this test.
type ExpectedUpstreamStateParamFormat struct {
	P string `json:"p"`
	U string `json:"u"`
	N string `json:"n"`
	C string `json:"c"`
	K string `json:"k"`
	V string `json:"v"`
}

type staticKeySet struct {
	publicKey crypto.PublicKey
}

func newStaticKeySet(publicKey crypto.PublicKey) coreosoidc.KeySet {
	return &staticKeySet{publicKey}
}

func (s *staticKeySet) VerifySignature(_ context.Context, jwt string) ([]byte, error) {
	jws, err := jose.ParseSigned(jwt)
	if err != nil {
		return nil, fmt.Errorf("oidc: malformed jwt: %w", err)
	}
	return jws.Verify(s.publicKey)
}

// VerifyECDSAIDToken verifies that the provided idToken was issued via the provided jwtSigningKey.
// It also performs some light validation on the claims, i.e., it makes sure the provided idToken
// has the provided  issuer and clientID.
//
// Further validation can be done via callers via the returned coreosoidc.IDToken.
func VerifyECDSAIDToken(
	t *testing.T,
	issuer, clientID string,
	jwtSigningKey *ecdsa.PrivateKey,
	idToken string,
) *coreosoidc.IDToken {
	t.Helper()

	keySet := newStaticKeySet(jwtSigningKey.Public())
	verifyConfig := coreosoidc.Config{ClientID: clientID, SupportedSigningAlgs: []string{coreosoidc.ES256}}
	verifier := coreosoidc.NewVerifier(issuer, keySet, &verifyConfig)
	token, err := verifier.Verify(context.Background(), idToken)
	require.NoError(t, err)

	return token
}

func RequireAuthCodeRegexpMatch(
	t *testing.T,
	actualContent string,
	wantRegexp string,
	kubeClient *fake.Clientset,
	secretsClient v1.SecretInterface,
	oauthStore fositestoragei.AllFositeStorage,
	wantDownstreamGrantedScopes []string,
	wantDownstreamIDTokenSubject string,
	wantDownstreamIDTokenUsername string,
	wantDownstreamIDTokenGroups []string,
	wantDownstreamRequestedScopes []string,
	wantDownstreamPKCEChallenge string,
	wantDownstreamPKCEChallengeMethod string,
	wantDownstreamNonce string,
	wantDownstreamClientID string,
	wantDownstreamRedirectURI string,
	wantCustomSessionData *psession.CustomSessionData,
) {
	t.Helper()

	// Assert that Location header matches regular expression.
	regex := regexp.MustCompile(wantRegexp)
	submatches := regex.FindStringSubmatch(actualContent)
	require.Lenf(t, submatches, 2, "no regexp match in actualContent: %", actualContent)
	capturedAuthCode := submatches[1]

	// fosite authcodes are in the format `data.signature`, so grab the signature part, which is the lookup key in the storage interface
	authcodeDataAndSignature := strings.Split(capturedAuthCode, ".")
	require.Len(t, authcodeDataAndSignature, 2)

	// Several Secrets should have been created
	expectedNumberOfCreatedSecrets := 2
	if includesOpenIDScope(wantDownstreamGrantedScopes) {
		expectedNumberOfCreatedSecrets++
	}
	require.Len(t, kubeClient.Actions(), expectedNumberOfCreatedSecrets)

	// One authcode should have been stored.
	testutil.RequireNumberOfSecretsMatchingLabelSelector(t, secretsClient, labels.Set{crud.SecretLabelKey: authorizationcode.TypeLabelValue}, 1)

	storedRequestFromAuthcode, storedSessionFromAuthcode := validateAuthcodeStorage(
		t,
		oauthStore,
		authcodeDataAndSignature[1], // Authcode store key is authcode signature
		wantDownstreamGrantedScopes,
		wantDownstreamIDTokenSubject,
		wantDownstreamIDTokenUsername,
		wantDownstreamIDTokenGroups,
		wantDownstreamRequestedScopes,
		wantDownstreamClientID,
		wantDownstreamRedirectURI,
		wantCustomSessionData,
	)

	// One PKCE should have been stored.
	testutil.RequireNumberOfSecretsMatchingLabelSelector(t, secretsClient, labels.Set{crud.SecretLabelKey: pkce2.TypeLabelValue}, 1)

	validatePKCEStorage(
		t,
		oauthStore,
		authcodeDataAndSignature[1], // PKCE store key is authcode signature
		storedRequestFromAuthcode,
		storedSessionFromAuthcode,
		wantDownstreamPKCEChallenge,
		wantDownstreamPKCEChallengeMethod,
	)

	// One IDSession should have been stored, if the downstream actually requested the "openid" scope
	if includesOpenIDScope(wantDownstreamGrantedScopes) {
		testutil.RequireNumberOfSecretsMatchingLabelSelector(t, secretsClient, labels.Set{crud.SecretLabelKey: openidconnect.TypeLabelValue}, 1)

		validateIDSessionStorage(
			t,
			oauthStore,
			capturedAuthCode, // IDSession store key is full authcode
			storedRequestFromAuthcode,
			storedSessionFromAuthcode,
			wantDownstreamNonce,
		)
	}
}

func includesOpenIDScope(scopes []string) bool {
	for _, scope := range scopes {
		if scope == "openid" {
			return true
		}
	}
	return false
}

func validateAuthcodeStorage(
	t *testing.T,
	oauthStore fositestoragei.AllFositeStorage,
	storeKey string,
	wantDownstreamGrantedScopes []string,
	wantDownstreamIDTokenSubject string,
	wantDownstreamIDTokenUsername string,
	wantDownstreamIDTokenGroups []string,
	wantDownstreamRequestedScopes []string,
	wantDownstreamClientID string,
	wantDownstreamRedirectURI string,
	wantCustomSessionData *psession.CustomSessionData,
) (*fosite.Request, *psession.PinnipedSession) {
	t.Helper()

	const (
		authCodeExpirationSeconds = 10 * 60 // Currently, we set our auth code expiration to 10 minutes
		timeComparisonFudgeFactor = time.Second * 15
	)

	// Get the authcode session back from storage so we can require that it was stored correctly.
	storedAuthorizeRequestFromAuthcode, err := oauthStore.GetAuthorizeCodeSession(context.Background(), storeKey, nil)
	require.NoError(t, err)

	// Check that storage returned the expected concrete data types.
	storedRequestFromAuthcode, storedSessionFromAuthcode := castStoredAuthorizeRequest(t, storedAuthorizeRequestFromAuthcode)

	// Check which scopes were granted.
	require.ElementsMatch(t, wantDownstreamGrantedScopes, storedRequestFromAuthcode.GetGrantedScopes())

	// Check all the other fields of the stored request.
	require.NotEmpty(t, storedRequestFromAuthcode.ID)
	require.Equal(t, wantDownstreamClientID, storedRequestFromAuthcode.Client.GetID())
	require.ElementsMatch(t, wantDownstreamRequestedScopes, storedRequestFromAuthcode.RequestedScope)
	require.Nil(t, storedRequestFromAuthcode.RequestedAudience)
	require.Empty(t, storedRequestFromAuthcode.GrantedAudience)
	require.Equal(t, url.Values{"redirect_uri": []string{wantDownstreamRedirectURI}}, storedRequestFromAuthcode.Form)
	testutil.RequireTimeInDelta(t, time.Now(), storedRequestFromAuthcode.RequestedAt, timeComparisonFudgeFactor)

	// We're not using these fields yet, so confirm that we did not set them (for now).
	require.Empty(t, storedSessionFromAuthcode.Fosite.Subject)
	require.Empty(t, storedSessionFromAuthcode.Fosite.Username)
	require.Empty(t, storedSessionFromAuthcode.Fosite.Headers)

	// The authcode that we are issuing should be good for the length of time that we declare in the fosite config.
	testutil.RequireTimeInDelta(t, time.Now().Add(authCodeExpirationSeconds*time.Second), storedSessionFromAuthcode.Fosite.ExpiresAt[fosite.AuthorizeCode], timeComparisonFudgeFactor)
	require.Len(t, storedSessionFromAuthcode.Fosite.ExpiresAt, 1)

	// Now confirm the ID token claims.
	actualClaims := storedSessionFromAuthcode.Fosite.Claims

	// Check the user's identity, which are put into the downstream ID token's subject, username and groups claims.
	require.Equal(t, wantDownstreamIDTokenSubject, actualClaims.Subject)
	require.Equal(t, wantDownstreamIDTokenUsername, actualClaims.Extra["username"])
	require.Len(t, actualClaims.Extra, 2)
	actualDownstreamIDTokenGroups := actualClaims.Extra["groups"]
	require.NotNil(t, actualDownstreamIDTokenGroups)
	require.ElementsMatch(t, wantDownstreamIDTokenGroups, actualDownstreamIDTokenGroups)

	// Check the rest of the downstream ID token's claims. Fosite wants us to set these (in UTC time).
	testutil.RequireTimeInDelta(t, time.Now().UTC(), actualClaims.RequestedAt, timeComparisonFudgeFactor)
	testutil.RequireTimeInDelta(t, time.Now().UTC(), actualClaims.AuthTime, timeComparisonFudgeFactor)
	requestedAtZone, _ := actualClaims.RequestedAt.Zone()
	require.Equal(t, "UTC", requestedAtZone)
	authTimeZone, _ := actualClaims.AuthTime.Zone()
	require.Equal(t, "UTC", authTimeZone)

	// Fosite will set these fields for us in the token endpoint based on the store session
	// information. Therefore, we assert that they are empty because we want the library to do the
	// lifting for us.
	require.Empty(t, actualClaims.Issuer)
	require.Nil(t, actualClaims.Audience)
	require.Empty(t, actualClaims.Nonce)
	require.Zero(t, actualClaims.ExpiresAt)
	require.Zero(t, actualClaims.IssuedAt)

	// These are not needed yet.
	require.Empty(t, actualClaims.JTI)
	require.Empty(t, actualClaims.CodeHash)
	require.Empty(t, actualClaims.AccessTokenHash)
	require.Empty(t, actualClaims.AuthenticationContextClassReference)
	require.Empty(t, actualClaims.AuthenticationMethodsReference)

	// Check that the custom Pinniped session data matches.
	require.Equal(t, wantCustomSessionData, storedSessionFromAuthcode.Custom)

	return storedRequestFromAuthcode, storedSessionFromAuthcode
}

func validatePKCEStorage(
	t *testing.T,
	oauthStore fositestoragei.AllFositeStorage,
	storeKey string,
	storedRequestFromAuthcode *fosite.Request,
	storedSessionFromAuthcode *psession.PinnipedSession,
	wantDownstreamPKCEChallenge, wantDownstreamPKCEChallengeMethod string,
) {
	t.Helper()

	storedAuthorizeRequestFromPKCE, err := oauthStore.GetPKCERequestSession(context.Background(), storeKey, nil)
	require.NoError(t, err)

	// Check that storage returned the expected concrete data types.
	storedRequestFromPKCE, storedSessionFromPKCE := castStoredAuthorizeRequest(t, storedAuthorizeRequestFromPKCE)

	// The stored PKCE request should be the same as the stored authcode request.
	require.Equal(t, storedRequestFromAuthcode.ID, storedRequestFromPKCE.ID)
	require.Equal(t, storedSessionFromAuthcode, storedSessionFromPKCE)

	// The stored PKCE request should also contain the PKCE challenge that the downstream sent us.
	require.Equal(t, wantDownstreamPKCEChallenge, storedRequestFromPKCE.Form.Get("code_challenge"))
	require.Equal(t, wantDownstreamPKCEChallengeMethod, storedRequestFromPKCE.Form.Get("code_challenge_method"))
}

func validateIDSessionStorage(
	t *testing.T,
	oauthStore fositestoragei.AllFositeStorage,
	storeKey string,
	storedRequestFromAuthcode *fosite.Request,
	storedSessionFromAuthcode *psession.PinnipedSession,
	wantDownstreamNonce string,
) {
	t.Helper()

	storedAuthorizeRequestFromIDSession, err := oauthStore.GetOpenIDConnectSession(context.Background(), storeKey, nil)
	require.NoError(t, err)

	// Check that storage returned the expected concrete data types.
	storedRequestFromIDSession, storedSessionFromIDSession := castStoredAuthorizeRequest(t, storedAuthorizeRequestFromIDSession)

	// The stored IDSession request should be the same as the stored authcode request.
	require.Equal(t, storedRequestFromAuthcode.ID, storedRequestFromIDSession.ID)
	require.Equal(t, storedSessionFromAuthcode, storedSessionFromIDSession)

	// The stored IDSession request should also contain the nonce that the downstream sent us.
	require.Equal(t, wantDownstreamNonce, storedRequestFromIDSession.Form.Get("nonce"))
}

func castStoredAuthorizeRequest(t *testing.T, storedAuthorizeRequest fosite.Requester) (*fosite.Request, *psession.PinnipedSession) {
	t.Helper()

	storedRequest, ok := storedAuthorizeRequest.(*fosite.Request)
	require.Truef(t, ok, "could not cast %T to %T", storedAuthorizeRequest, &fosite.Request{})
	storedSession, ok := storedAuthorizeRequest.GetSession().(*psession.PinnipedSession)
	require.Truef(t, ok, "could not cast %T to %T", storedAuthorizeRequest.GetSession(), &psession.PinnipedSession{})

	return storedRequest, storedSession
}
