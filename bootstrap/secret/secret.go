/********************************************************************************
 *  Copyright 2019 Dell Inc.
 *  Copyright 2022 Intel Corp.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License. You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software distributed under the License
 * is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
 * or implied. See the License for the specific language governing permissions and limitations under
 * the License.
 *******************************************************************************/

package secret

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/edgexfoundry/go-mod-bootstrap/v3/bootstrap/environment"
	"github.com/edgexfoundry/go-mod-bootstrap/v3/config"
	"github.com/edgexfoundry/go-mod-core-contracts/v3/clients/logger"
	"github.com/edgexfoundry/go-mod-secrets/v3/pkg/types"
	"github.com/edgexfoundry/go-mod-secrets/v3/secrets"

	"github.com/edgexfoundry/go-mod-bootstrap/v3/bootstrap/container"
	"github.com/edgexfoundry/go-mod-bootstrap/v3/bootstrap/interfaces"
	"github.com/edgexfoundry/go-mod-bootstrap/v3/bootstrap/startup"
	"github.com/edgexfoundry/go-mod-bootstrap/v3/di"

	"github.com/edgexfoundry/go-mod-secrets/v3/pkg/token/authtokenloader"
	"github.com/edgexfoundry/go-mod-secrets/v3/pkg/token/fileioperformer"
	"github.com/edgexfoundry/go-mod-secrets/v3/pkg/token/runtimetokenprovider"
)

// secret service Metric Names
const (
	secretsRequestedMetricName        = "SecuritySecretsRequested"
	secretsStoredMetricName           = "SecuritySecretsStored"
	securityConsulTokensRequestedName = "SecurityConsulTokensRequested"
	securityConsulTokenDurationName   = "SecurityConsulTokenDuration"
)

// NewSecretProvider creates a new fully initialized the Secret Provider.
func NewSecretProvider(
	configuration interfaces.Configuration,
	envVars *environment.Variables,
	ctx context.Context,
	startupTimer startup.Timer,
	dic *di.Container,
	serviceKey string) (interfaces.SecretProviderExt, error) {
	lc := container.LoggingClientFrom(dic.Get)

	var provider interfaces.SecretProviderExt

	switch IsSecurityEnabled() {
	case true:
		// attempt to create a new Secure client only if security is enabled.
		var err error

		lc.Info("Creating SecretClient")

		secretStoreConfig, err := BuildSecretStoreConfig(serviceKey, envVars, lc)
		if err != nil {
			return nil, err
		}

		for startupTimer.HasNotElapsed() {
			var secretConfig types.SecretConfig

			lc.Info("Reading secret store configuration and authentication token")

			tokenLoader := container.AuthTokenLoaderFrom(dic.Get)
			if tokenLoader == nil {
				tokenLoader = authtokenloader.NewAuthTokenLoader(fileioperformer.NewDefaultFileIoPerformer())
			}

			runtimeTokenLoader := container.RuntimeTokenProviderFrom(dic.Get)
			if runtimeTokenLoader == nil {
				runtimeTokenLoader = runtimetokenprovider.NewRuntimeTokenProvider(ctx, lc,
					secretStoreConfig.RuntimeTokenProvider)
			}

			secretConfig, err = getSecretConfig(secretStoreConfig, tokenLoader, runtimeTokenLoader, serviceKey, lc)
			if err == nil {
				secureProvider := NewSecureProvider(ctx, secretStoreConfig, lc, tokenLoader, runtimeTokenLoader, serviceKey)
				var secretClient secrets.SecretClient

				lc.Info("Attempting to create secret client")

				tokenCallbackFunc := secureProvider.DefaultTokenExpiredCallback
				if secretConfig.RuntimeTokenProvider.Enabled {
					tokenCallbackFunc = secureProvider.RuntimeTokenExpiredCallback
				}

				secretClient, err = secrets.NewSecretsClient(ctx, secretConfig, lc, tokenCallbackFunc)
				if err == nil {
					secureProvider.SetClient(secretClient)
					provider = secureProvider
					lc.Info("Created SecretClient")

					lc.Debugf("SecretsFile is '%s'", secretConfig.SecretsFile)

					if len(strings.TrimSpace(secretConfig.SecretsFile)) == 0 {
						lc.Infof("SecretsFile not set, skipping seeding of service secrets.")
						break
					}

					provider = secureProvider
					lc.Info("Created SecretClient")

					err = secureProvider.LoadServiceSecrets(secretStoreConfig)
					if err != nil {
						return nil, err
					}
					break
				}
			}

			lc.Warn(fmt.Sprintf("Retryable failure while creating SecretClient: %s", err.Error()))
			startupTimer.SleepForInterval()
		}

		if err != nil {
			return nil, fmt.Errorf("unable to create SecretClient: %s", err.Error())
		}

	case false:
		provider = NewInsecureProvider(configuration, lc)
	}

	dic.Update(di.ServiceConstructorMap{
		// Must put the SecretProvider instance in the DIC for both the standard API use by service code
		// and the extended API used by boostrap code
		container.SecretProviderName: func(get di.Get) interface{} {
			return provider
		},
		container.SecretProviderExtName: func(get di.Get) interface{} {
			return provider
		},
	})

	return provider, nil
}

// BuildSecretStoreConfig is public helper function that builds the SecretStore configuration
// from default values and  environment override.
func BuildSecretStoreConfig(serviceKey string, envVars *environment.Variables, lc logger.LoggingClient) (*config.SecretStoreInfo, error) {
	configWrapper := struct {
		SecretStore config.SecretStoreInfo
	}{
		SecretStore: config.NewSecretStoreInfo(serviceKey),
	}

	count, err := envVars.OverrideConfiguration(&configWrapper)
	if err != nil {
		return nil, fmt.Errorf("failed to override SecretStore information: %v", err)
	}

	lc.Infof("SecretStore information created with %d overrides applied", count)
	return &configWrapper.SecretStore, nil
}

// getSecretConfig creates a SecretConfig based on the SecretStoreInfo configuration properties.
// If a token file is present it will override the Authentication.AuthToken value.
func getSecretConfig(secretStoreInfo *config.SecretStoreInfo,
	tokenLoader authtokenloader.AuthTokenLoader,
	runtimeTokenLoader runtimetokenprovider.RuntimeTokenProvider,
	serviceKey string,
	lc logger.LoggingClient) (types.SecretConfig, error) {
	secretConfig := types.SecretConfig{
		Type:                 secretStoreInfo.Type, // Type of SecretStore implementation, i.e. Vault
		Host:                 secretStoreInfo.Host,
		Port:                 secretStoreInfo.Port,
		BasePath:             addEdgeXSecretNamePrefix(secretStoreInfo.StoreName),
		SecretsFile:          secretStoreInfo.SecretsFile,
		Protocol:             secretStoreInfo.Protocol,
		Namespace:            secretStoreInfo.Namespace,
		RootCaCertPath:       secretStoreInfo.RootCaCertPath,
		ServerName:           secretStoreInfo.ServerName,
		Authentication:       secretStoreInfo.Authentication,
		RuntimeTokenProvider: secretStoreInfo.RuntimeTokenProvider,
	}

	// maybe insecure mode
	// if both configs of token file and runtime token provider are empty or disabled
	// then we treat that as insecure mode
	if !IsSecurityEnabled() || (secretStoreInfo.TokenFile == "" && !secretConfig.RuntimeTokenProvider.Enabled) {
		lc.Info("insecure mode")
		return secretConfig, nil
	}

	// based on whether token provider config is configured or not, we will obtain token in different way
	var token string
	var err error
	if secretConfig.RuntimeTokenProvider.Enabled {
		lc.Info("runtime token provider enabled")
		// call spiffe token provider to get token on the fly
		token, err = runtimeTokenLoader.GetRawToken(serviceKey)
	} else {
		lc.Info("load token from file")
		// else obtain the token from TokenFile
		token, err = tokenLoader.Load(secretStoreInfo.TokenFile)
	}

	if err != nil {
		return secretConfig, err
	}

	secretConfig.Authentication.AuthToken = token
	return secretConfig, nil
}

func addEdgeXSecretNamePrefix(secretName string) string {
	trimmedSecretName := strings.TrimSpace(secretName)

	// in this case, treat it as no secret name prefix
	if len(trimmedSecretName) == 0 {
		return ""
	}

	return "/" + path.Join("v1", "secret", "edgex", trimmedSecretName)
}
