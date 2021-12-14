// Copyright 2020-2021 the Pinniped contributors. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"net/url"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"k8s.io/apimachinery/pkg/types"

	"go.pinniped.dev/internal/authenticators"
	"go.pinniped.dev/pkg/oidcclient/nonce"
	"go.pinniped.dev/pkg/oidcclient/oidctypes"
	"go.pinniped.dev/pkg/oidcclient/pkce"
)

type UpstreamOIDCIdentityProviderI interface {
	// GetName returns a name for this upstream provider, which will be used as a component of the path for the
	// callback endpoint hosted by the Supervisor.
	GetName() string

	// GetClientID returns the OAuth client ID registered with the upstream provider to be used in the authorization code flow.
	GetClientID() string

	// GetResourceUID returns the Kubernetes resource ID
	GetResourceUID() types.UID

	// GetAuthorizationURL returns the Authorization Endpoint fetched from discovery.
	GetAuthorizationURL() *url.URL

	// GetScopes returns the scopes to request in authorization (authcode or password grant) flow.
	GetScopes() []string

	// GetUsernameClaim returns the ID Token username claim name. May return empty string, in which case we
	// will use some reasonable defaults.
	GetUsernameClaim() string

	// GetGroupsClaim returns the ID Token groups claim name. May return empty string, in which case we won't
	// try to read groups from the upstream provider.
	GetGroupsClaim() string

	// AllowsPasswordGrant returns true if a client should be allowed to use the resource owner password credentials grant
	// flow with this upstream provider. When false, it should not be allowed.
	AllowsPasswordGrant() bool

	// GetAdditionalAuthcodeParams returns additional params to be sent on authcode requests.
	GetAdditionalAuthcodeParams() map[string]string

	// PasswordCredentialsGrantAndValidateTokens performs upstream OIDC resource owner password credentials grant and
	// token validation. Returns the validated raw tokens as well as the parsed claims of the ID token.
	PasswordCredentialsGrantAndValidateTokens(ctx context.Context, username, password string) (*oidctypes.Token, error)

	// ExchangeAuthcodeAndValidateTokens performs upstream OIDC authorization code exchange and token validation.
	// Returns the validated raw tokens as well as the parsed claims of the ID token.
	ExchangeAuthcodeAndValidateTokens(
		ctx context.Context,
		authcode string,
		pkceCodeVerifier pkce.Code,
		expectedIDTokenNonce nonce.Nonce,
		redirectURI string,
	) (*oidctypes.Token, error)

	// PerformRefresh will call the provider's token endpoint to perform a refresh grant. The provider may or may not
	// return a new ID or refresh token in the response. If it returns an ID token, then use ValidateToken to
	// validate the ID token.
	PerformRefresh(ctx context.Context, refreshToken string) (*oauth2.Token, error)

	// RevokeRefreshToken will attempt to revoke the given token, if the provider has a revocation endpoint.
	RevokeRefreshToken(ctx context.Context, refreshToken string) error

	// ValidateToken will validate the ID token. It will also merge the claims from the userinfo endpoint response
	// into the ID token's claims, if the provider offers the userinfo endpoint. It returns the validated/updated
	// tokens, or an error.
	ValidateToken(ctx context.Context, tok *oauth2.Token, expectedIDTokenNonce nonce.Nonce, requireIDToken bool) (*oidctypes.Token, error)
}

type UpstreamLDAPIdentityProviderI interface {
	// GetName returns a name for this upstream provider.
	GetName() string

	// GetURL returns a URL which uniquely identifies this LDAP provider, e.g. "ldaps://host.example.com:1234".
	// This URL is not used for connecting to the provider, but rather is used for creating a globally unique user
	// identifier by being combined with the user's UID, since user UIDs are only unique within one provider.
	GetURL() *url.URL

	// GetResourceUID returns the Kubernetes resource ID
	GetResourceUID() types.UID

	// UserAuthenticator adds an interface method for performing user authentication against the upstream LDAP provider.
	authenticators.UserAuthenticator

	// PerformRefresh performs a refresh against the upstream LDAP identity provider
	PerformRefresh(ctx context.Context, storedRefreshAttributes StoredRefreshAttributes) error
}

type StoredRefreshAttributes struct {
	Username             string
	Subject              string
	DN                   string
	AuthTime             time.Time
	AdditionalAttributes map[string]string
}

type DynamicUpstreamIDPProvider interface {
	SetOIDCIdentityProviders(oidcIDPs []UpstreamOIDCIdentityProviderI)
	GetOIDCIdentityProviders() []UpstreamOIDCIdentityProviderI
	SetLDAPIdentityProviders(ldapIDPs []UpstreamLDAPIdentityProviderI)
	GetLDAPIdentityProviders() []UpstreamLDAPIdentityProviderI
	SetActiveDirectoryIdentityProviders(adIDPs []UpstreamLDAPIdentityProviderI)
	GetActiveDirectoryIdentityProviders() []UpstreamLDAPIdentityProviderI
}

type dynamicUpstreamIDPProvider struct {
	oidcUpstreams            []UpstreamOIDCIdentityProviderI
	ldapUpstreams            []UpstreamLDAPIdentityProviderI
	activeDirectoryUpstreams []UpstreamLDAPIdentityProviderI
	mutex                    sync.RWMutex
}

func NewDynamicUpstreamIDPProvider() DynamicUpstreamIDPProvider {
	return &dynamicUpstreamIDPProvider{
		oidcUpstreams:            []UpstreamOIDCIdentityProviderI{},
		ldapUpstreams:            []UpstreamLDAPIdentityProviderI{},
		activeDirectoryUpstreams: []UpstreamLDAPIdentityProviderI{},
	}
}

func (p *dynamicUpstreamIDPProvider) SetOIDCIdentityProviders(oidcIDPs []UpstreamOIDCIdentityProviderI) {
	p.mutex.Lock() // acquire a write lock
	defer p.mutex.Unlock()
	p.oidcUpstreams = oidcIDPs
}

func (p *dynamicUpstreamIDPProvider) GetOIDCIdentityProviders() []UpstreamOIDCIdentityProviderI {
	p.mutex.RLock() // acquire a read lock
	defer p.mutex.RUnlock()
	return p.oidcUpstreams
}

func (p *dynamicUpstreamIDPProvider) SetLDAPIdentityProviders(ldapIDPs []UpstreamLDAPIdentityProviderI) {
	p.mutex.Lock() // acquire a write lock
	defer p.mutex.Unlock()
	p.ldapUpstreams = ldapIDPs
}

func (p *dynamicUpstreamIDPProvider) GetLDAPIdentityProviders() []UpstreamLDAPIdentityProviderI {
	p.mutex.RLock() // acquire a read lock
	defer p.mutex.RUnlock()
	return p.ldapUpstreams
}

func (p *dynamicUpstreamIDPProvider) SetActiveDirectoryIdentityProviders(adIDPs []UpstreamLDAPIdentityProviderI) {
	p.mutex.Lock() // acquire a write lock
	defer p.mutex.Unlock()
	p.activeDirectoryUpstreams = adIDPs
}

func (p *dynamicUpstreamIDPProvider) GetActiveDirectoryIdentityProviders() []UpstreamLDAPIdentityProviderI {
	p.mutex.RLock() // acquire a read lock
	defer p.mutex.RUnlock()
	return p.activeDirectoryUpstreams
}
