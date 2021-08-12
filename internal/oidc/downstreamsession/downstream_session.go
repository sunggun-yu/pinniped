// Copyright 2021 the Pinniped contributors. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Package downstreamsession provides some shared helpers for creating downstream OIDC sessions.
package downstreamsession

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	oidc2 "github.com/coreos/go-oidc/v3/oidc"
	"github.com/ory/fosite"
	"github.com/ory/fosite/handler/openid"
	"github.com/ory/fosite/token/jwt"

	"go.pinniped.dev/internal/httputil/httperr"
	"go.pinniped.dev/internal/oidc"
	"go.pinniped.dev/internal/oidc/provider"
	"go.pinniped.dev/internal/plog"
)

const (
	// The name of the email claim from https://openid.net/specs/openid-connect-core-1_0.html#StandardClaims
	emailClaimName = "email"

	// The name of the email_verified claim from https://openid.net/specs/openid-connect-core-1_0.html#StandardClaims
	emailVerifiedClaimName = "email_verified"
)

// MakeDownstreamSession creates a downstream OIDC session.
func MakeDownstreamSession(subject string, username string, groups []string) *openid.DefaultSession {
	now := time.Now().UTC()
	openIDSession := &openid.DefaultSession{
		Claims: &jwt.IDTokenClaims{
			Subject:     subject,
			RequestedAt: now,
			AuthTime:    now,
		},
	}
	if groups == nil {
		groups = []string{}
	}
	openIDSession.Claims.Extra = map[string]interface{}{
		oidc.DownstreamUsernameClaim: username,
		oidc.DownstreamGroupsClaim:   groups,
	}
	return openIDSession
}

// GrantScopesIfRequested auto-grants the scopes for which we do not require end-user approval, if they were requested.
func GrantScopesIfRequested(authorizeRequester fosite.AuthorizeRequester) {
	oidc.GrantScopeIfRequested(authorizeRequester, oidc2.ScopeOpenID)
	oidc.GrantScopeIfRequested(authorizeRequester, oidc2.ScopeOfflineAccess)
	oidc.GrantScopeIfRequested(authorizeRequester, "pinniped:request-audience")
}

func GetSubjectAndUsernameFromUpstreamIDToken(
	upstreamIDPConfig provider.UpstreamOIDCIdentityProviderI,
	idTokenClaims map[string]interface{},
) (string, string, error) {
	// The spec says the "sub" claim is only unique per issuer,
	// so we will prepend the issuer string to make it globally unique.
	upstreamIssuer := idTokenClaims[oidc.IDTokenIssuerClaim]
	if upstreamIssuer == "" {
		plog.Warning(
			"issuer claim in upstream ID token missing",
			"upstreamName", upstreamIDPConfig.GetName(),
			"issClaim", upstreamIssuer,
		)
		return "", "", httperr.New(http.StatusUnprocessableEntity, "issuer claim in upstream ID token missing")
	}
	upstreamIssuerAsString, ok := upstreamIssuer.(string)
	if !ok {
		plog.Warning(
			"issuer claim in upstream ID token has invalid format",
			"upstreamName", upstreamIDPConfig.GetName(),
			"issClaim", upstreamIssuer,
		)
		return "", "", httperr.New(http.StatusUnprocessableEntity, "issuer claim in upstream ID token has invalid format")
	}

	subjectAsInterface, ok := idTokenClaims[oidc.IDTokenSubjectClaim]
	if !ok {
		plog.Warning(
			"no subject claim in upstream ID token",
			"upstreamName", upstreamIDPConfig.GetName(),
		)
		return "", "", httperr.New(http.StatusUnprocessableEntity, "no subject claim in upstream ID token")
	}

	upstreamSubject, ok := subjectAsInterface.(string)
	if !ok {
		plog.Warning(
			"subject claim in upstream ID token has invalid format",
			"upstreamName", upstreamIDPConfig.GetName(),
		)
		return "", "", httperr.New(http.StatusUnprocessableEntity, "subject claim in upstream ID token has invalid format")
	}

	subject := downstreamSubjectFromUpstreamOIDC(upstreamIssuerAsString, upstreamSubject)

	usernameClaimName := upstreamIDPConfig.GetUsernameClaim()
	if usernameClaimName == "" {
		return subject, subject, nil
	}

	// If the upstream username claim is configured to be the special "email" claim and the upstream "email_verified"
	// claim is present, then validate that the "email_verified" claim is true.
	emailVerifiedAsInterface, ok := idTokenClaims[emailVerifiedClaimName]
	if usernameClaimName == emailClaimName && ok {
		emailVerified, ok := emailVerifiedAsInterface.(bool)
		if !ok {
			plog.Warning(
				"username claim configured as \"email\" and upstream email_verified claim is not a boolean",
				"upstreamName", upstreamIDPConfig.GetName(),
				"configuredUsernameClaim", usernameClaimName,
				"emailVerifiedClaim", emailVerifiedAsInterface,
			)
			return "", "", httperr.New(http.StatusUnprocessableEntity, "email_verified claim in upstream ID token has invalid format")
		}
		if !emailVerified {
			plog.Warning(
				"username claim configured as \"email\" and upstream email_verified claim has false value",
				"upstreamName", upstreamIDPConfig.GetName(),
				"configuredUsernameClaim", usernameClaimName,
			)
			return "", "", httperr.New(http.StatusUnprocessableEntity, "email_verified claim in upstream ID token has false value")
		}
	}

	usernameAsInterface, ok := idTokenClaims[usernameClaimName]
	if !ok {
		plog.Warning(
			"no username claim in upstream ID token",
			"upstreamName", upstreamIDPConfig.GetName(),
			"configuredUsernameClaim", usernameClaimName,
		)
		return "", "", httperr.New(http.StatusUnprocessableEntity, "no username claim in upstream ID token")
	}

	username, ok := usernameAsInterface.(string)
	if !ok {
		plog.Warning(
			"username claim in upstream ID token has invalid format",
			"upstreamName", upstreamIDPConfig.GetName(),
			"configuredUsernameClaim", usernameClaimName,
		)
		return "", "", httperr.New(http.StatusUnprocessableEntity, "username claim in upstream ID token has invalid format")
	}

	return subject, username, nil
}

func downstreamSubjectFromUpstreamOIDC(upstreamIssuerAsString string, upstreamSubject string) string {
	return fmt.Sprintf("%s?%s=%s", upstreamIssuerAsString, oidc.IDTokenSubjectClaim, url.QueryEscape(upstreamSubject))
}

func GetGroupsFromUpstreamIDToken(
	upstreamIDPConfig provider.UpstreamOIDCIdentityProviderI,
	idTokenClaims map[string]interface{},
) ([]string, error) {
	groupsClaimName := upstreamIDPConfig.GetGroupsClaim()
	if groupsClaimName == "" {
		return nil, nil
	}

	groupsAsInterface, ok := idTokenClaims[groupsClaimName]
	if !ok {
		plog.Warning(
			"no groups claim in upstream ID token",
			"upstreamName", upstreamIDPConfig.GetName(),
			"configuredGroupsClaim", groupsClaimName,
		)
		return nil, nil // the upstream IDP may have omitted the claim if the user has no groups
	}

	groupsAsArray, okAsArray := extractGroups(groupsAsInterface)
	if !okAsArray {
		plog.Warning(
			"groups claim in upstream ID token has invalid format",
			"upstreamName", upstreamIDPConfig.GetName(),
			"configuredGroupsClaim", groupsClaimName,
		)
		return nil, httperr.New(http.StatusUnprocessableEntity, "groups claim in upstream ID token has invalid format")
	}

	return groupsAsArray, nil
}

func extractGroups(groupsAsInterface interface{}) ([]string, bool) {
	groupsAsString, okAsString := groupsAsInterface.(string)
	if okAsString {
		return []string{groupsAsString}, true
	}

	groupsAsStringArray, okAsStringArray := groupsAsInterface.([]string)
	if okAsStringArray {
		return groupsAsStringArray, true
	}

	groupsAsInterfaceArray, okAsArray := groupsAsInterface.([]interface{})
	if !okAsArray {
		return nil, false
	}

	var groupsAsStrings []string
	for _, groupAsInterface := range groupsAsInterfaceArray {
		groupAsString, okAsString := groupAsInterface.(string)
		if !okAsString {
			return nil, false
		}
		if groupAsString != "" {
			groupsAsStrings = append(groupsAsStrings, groupAsString)
		}
	}

	return groupsAsStrings, true
}
