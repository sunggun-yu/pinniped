// Copyright 2020-2021 the Pinniped contributors. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/spf13/cobra"
	clientauthv1beta1 "k8s.io/client-go/pkg/apis/clientauthentication/v1beta1"

	authenticationv1alpha1 "go.pinniped.dev/generated/1.20/apis/concierge/authentication/v1alpha1"
	loginv1alpha1 "go.pinniped.dev/generated/1.20/apis/concierge/login/v1alpha1"
	"go.pinniped.dev/pkg/conciergeclient"
	"go.pinniped.dev/pkg/oidcclient/oidctypes"
)

//nolint: gochecknoinits
func init() {
	loginCmd.AddCommand(staticLoginCommand(staticLoginRealDeps()))
}

type staticLoginDeps struct {
	lookupEnv     func(string) (string, bool)
	exchangeToken func(context.Context, *conciergeclient.Client, string) (*clientauthv1beta1.ExecCredential, error)
}

func staticLoginRealDeps() staticLoginDeps {
	return staticLoginDeps{
		lookupEnv: os.LookupEnv,
		exchangeToken: func(ctx context.Context, client *conciergeclient.Client, token string) (*clientauthv1beta1.ExecCredential, error) {
			return client.ExchangeToken(ctx, token)
		},
	}
}

type staticLoginParams struct {
	staticToken                string
	staticTokenEnvName         string
	conciergeEnabled           bool
	conciergeNamespace         string
	conciergeAuthenticatorType string
	conciergeAuthenticatorName string
	conciergeEndpoint          string
	conciergeCABundle          string
	conciergeAPIGroupSuffix    string
	useImpersonationProxy      bool
}

func staticLoginCommand(deps staticLoginDeps) *cobra.Command {
	var (
		cmd = cobra.Command{
			Args:         cobra.NoArgs,
			Use:          "static [--token TOKEN] [--token-env TOKEN_NAME]",
			Short:        "Login using a static token",
			SilenceUsage: true,
		}
		flags staticLoginParams
	)
	cmd.Flags().StringVar(&flags.staticToken, "token", "", "Static token to present during login")
	cmd.Flags().StringVar(&flags.staticTokenEnvName, "token-env", "", "Environment variable containing a static token")
	cmd.Flags().BoolVar(&flags.conciergeEnabled, "enable-concierge", false, "Exchange the token with the Pinniped concierge during login")
	cmd.Flags().StringVar(&flags.conciergeNamespace, "concierge-namespace", "pinniped-concierge", "Namespace in which the concierge was installed")
	cmd.Flags().StringVar(&flags.conciergeAuthenticatorType, "concierge-authenticator-type", "", "Concierge authenticator type (e.g., 'webhook', 'jwt')")
	cmd.Flags().StringVar(&flags.conciergeAuthenticatorName, "concierge-authenticator-name", "", "Concierge authenticator name")
	cmd.Flags().StringVar(&flags.conciergeEndpoint, "concierge-endpoint", "", "API base for the Pinniped concierge endpoint")
	cmd.Flags().StringVar(&flags.conciergeCABundle, "concierge-ca-bundle-data", "", "CA bundle to use when connecting to the concierge")
	cmd.Flags().StringVar(&flags.conciergeAPIGroupSuffix, "concierge-api-group-suffix", "pinniped.dev", "Concierge API group suffix")
	cmd.Flags().BoolVar(&flags.useImpersonationProxy, "concierge-use-impersonation-proxy", false, "Whether the concierge cluster uses an impersonation proxy")
	cmd.RunE = func(cmd *cobra.Command, args []string) error { return runStaticLogin(cmd.OutOrStdout(), deps, flags) }
	return &cmd
}

func runStaticLogin(out io.Writer, deps staticLoginDeps, flags staticLoginParams) error {
	if flags.staticToken == "" && flags.staticTokenEnvName == "" {
		return fmt.Errorf("one of --token or --token-env must be set")
	}

	var concierge *conciergeclient.Client
	if flags.conciergeEnabled {
		var err error
		concierge, err = conciergeclient.New(
			conciergeclient.WithNamespace(flags.conciergeNamespace),
			conciergeclient.WithEndpoint(flags.conciergeEndpoint),
			conciergeclient.WithBase64CABundle(flags.conciergeCABundle),
			conciergeclient.WithAuthenticator(flags.conciergeAuthenticatorType, flags.conciergeAuthenticatorName),
			conciergeclient.WithAPIGroupSuffix(flags.conciergeAPIGroupSuffix),
		)
		if err != nil {
			return fmt.Errorf("invalid concierge parameters: %w", err)
		}
	}

	var token string
	if flags.staticToken != "" {
		token = flags.staticToken
	}
	if flags.staticTokenEnvName != "" {
		var ok bool
		token, ok = deps.lookupEnv(flags.staticTokenEnvName)
		if !ok {
			return fmt.Errorf("--token-env variable %q is not set", flags.staticTokenEnvName)
		}
		if token == "" {
			return fmt.Errorf("--token-env variable %q is empty", flags.staticTokenEnvName)
		}
	}
	cred := tokenCredential(&oidctypes.Token{IDToken: &oidctypes.IDToken{Token: token}})

	// Exchange that token with the concierge, if configured.
	if concierge != nil && !flags.useImpersonationProxy {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		var err error
		cred, err = deps.exchangeToken(ctx, concierge, token)
		if err != nil {
			return fmt.Errorf("could not complete concierge credential exchange: %w", err)
		}
	}
	if concierge != nil && flags.useImpersonationProxy {
		// Put the token into a TokenCredentialRequest
		// put the TokenCredentialRequest in an ExecCredential
		req, err := execCredentialForImpersonationProxyStatic(token, flags)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(req)
	}
	return json.NewEncoder(out).Encode(cred)
}

func execCredentialForImpersonationProxyStatic(token string, flags staticLoginParams) (*clientauthv1beta1.ExecCredential, error) {
	// TODO maybe de-dup this with conciergeclient.go
	var kind string
	switch strings.ToLower(flags.conciergeAuthenticatorType) {
	case "webhook":
		kind = "WebhookAuthenticator"
	case "jwt":
		kind = "JWTAuthenticator"
	default:
		return nil, fmt.Errorf(`invalid authenticator type: %q, supported values are "webhook" and "jwt"`, kind)
	}
	reqJSON, err := json.Marshal(&loginv1alpha1.TokenCredentialRequest{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: flags.conciergeNamespace,
		},
		TypeMeta: metav1.TypeMeta{
			Kind:       "TokenCredentialRequest",
			APIVersion: loginv1alpha1.GroupName + "/v1alpha1",
		},
		Spec: loginv1alpha1.TokenCredentialRequestSpec{
			Token: token, // TODO
			Authenticator: corev1.TypedLocalObjectReference{
				APIGroup: &authenticationv1alpha1.SchemeGroupVersion.Group,
				Kind:     kind,
				Name:     flags.conciergeAuthenticatorName,
			},
		},
	})
	if err != nil {
		return nil, err
	}
	encodedToken := base64.RawURLEncoding.EncodeToString(reqJSON)
	cred := &clientauthv1beta1.ExecCredential{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ExecCredential",
			APIVersion: "client.authentication.k8s.io/v1beta1",
		},
		Status: &clientauthv1beta1.ExecCredentialStatus{
			Token: encodedToken,
		},
	}
	return cred, nil
}
