package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/hashicorp/vault/api"
	log "github.com/sirupsen/logrus"
	"github.com/tuenti/secrets-manager/errors"
)

var (
	logger  *log.Logger
	metrics *vaultMetrics
)

const defaultSecretKey = "data"

type client struct {
	vclient            *api.Client
	logical            *api.Logical
	maxTokenTTL        int64
	tokenPollingPeriod time.Duration
	renewTTLIncrement  int
	engine             engine
}

func vaultClient(ctx context.Context, l *log.Logger, cfg Config) (*client, error) {
	if l != nil {
		logger = l
	} else {
		logger = log.New()
	}

	httpClient := new(http.Client)
	vclient, err := api.NewClient(&api.Config{Address: cfg.VaultURL, HttpClient: httpClient})

	if err != nil {
		logger.Debugf("unable to build vault client: %v", err)
		return nil, err
	}

	vclient.SetToken(cfg.VaultToken)
	sys := vclient.Sys()
	health, err := sys.Health()

	if err != nil {
		logger.Debugf("could not contact Vault at %s: %v ", cfg.VaultURL, err)
		return nil, err
	}

	logger.Infof("successfully logged into Vault cluster %s", health.ClusterName)
	logical := vclient.Logical()

	engine, err := newEngine(cfg.VaultEngine)
	if err != nil {
		logger.Debugf("unable to use engine %s: %v", cfg.VaultEngine, err)
		return nil, err
	}

	metrics = newVaultMetrics(cfg.VaultURL, health.Version, cfg.VaultEngine, health.ClusterID, health.ClusterName)

	client := client{
		vclient:            vclient,
		logical:            logical,
		maxTokenTTL:        cfg.VaultMaxTokenTTL,
		tokenPollingPeriod: cfg.VaultTokenPollingPeriod,
		renewTTLIncrement:  cfg.VaultRenewTTLIncrement,
		engine:             engine,
	}
	client.startTokenRenewer(ctx)
	return &client, err
}

func (c *client) getToken() (*api.Secret, error) {
	auth := c.vclient.Auth()
	lookup, err := auth.Token().LookupSelf()
	if err != nil {
		logger.Errorf("error checking token with lookup self api: %v", err)
		metrics.updateVaultTokenLookupErrorsCountMetric(errors.UnknownErrorType)
		return nil, err
	}
	return lookup, nil
}

func (c *client) getTokenTTL(token *api.Secret) (int64, error) {
	var ttl int64
	ttl, err := token.Data["ttl"].(json.Number).Int64()
	if err != nil {
		logger.Errorf("couldn't decode ttl from token: %v", err)
		return -1, err
	}
	metrics.updateVaultTokenTTLMetric(ttl)
	return ttl, nil
}

func (c *client) shouldRenewToken(ttl int64) bool {
	if ttl < c.maxTokenTTL {
		metrics.updateVaultTokenExpiredMetric(vaultTokenExpired)
		return true
	}
	metrics.updateVaultTokenExpiredMetric(vaultTokenNotExpired)
	return false
}

func (c *client) renewToken(token *api.Secret) error {
	isRenewable, err := token.TokenIsRenewable()
	if err != nil {
		logger.Errorf("could not check token renewability: %v", err)
		metrics.updateVaultTokenRenewErrorsCountMetric(errors.UnknownErrorType)
		return err
	}
	if !isRenewable {
		metrics.updateVaultTokenRenewErrorsCountMetric(errors.VaultTokenNotRenewableErrorType)
		err = &errors.VaultTokenNotRenewableError{ErrType: errors.VaultTokenNotRenewableErrorType}
		return err
	}
	auth := c.vclient.Auth()
	if _, err = auth.Token().RenewSelf(c.renewTTLIncrement); err != nil {
		log.Errorf("failed to renew token: %v", err)
		metrics.updateVaultTokenRenewErrorsCountMetric(errors.UnknownErrorType)
		return err
	}
	return nil
}

func (c *client) startTokenRenewer(ctx context.Context) {
	go func(ctx context.Context) {
		for {
			select {
			case <-time.After(c.tokenPollingPeriod):
				token, err := c.getToken()
				if err != nil {
					logger.Errorf("failed to fetch token: %v", err)
					return
				}
				ttl, err := c.getTokenTTL(token)
				if err != nil {
					logger.Errorf("failed to read token TTL: %v", err)
					return
				} else if c.shouldRenewToken(ttl) {
					logger.Warnf("token is really close to expire, current ttl: %d", ttl)
					err := c.renewToken(token)
					if err != nil {
						logger.Errorf("could not renew token: %v", err)
					} else {
						logger.Infoln("token renewed successfully!")
					}
				} else {
					return
				}
			case <-ctx.Done():
				logger.Infoln("gracefully shutting down token renewal go routine")
				return
			}
		}
	}(ctx)
}

func (c *client) ReadSecret(path string, key string) (string, error) {
	data := ""
	if key == "" {
		key = defaultSecretKey
	}

	logical := c.logical
	secret, err := logical.Read(path)
	if err != nil {
		metrics.updateVaultSecretReadErrorsCountMetric(path, key, errors.UnknownErrorType)
		return data, err
	}

	if secret != nil {
		secretData := c.engine.getData(secret)
		warnings := secret.Warnings
		if secretData != nil {
			if secretData[key] != nil {
				data = secretData[key].(string)
			} else {
				metrics.updateVaultSecretReadErrorsCountMetric(path, key, errors.BackendSecretNotFoundErrorType)
				err = &errors.BackendSecretNotFoundError{ErrType: errors.BackendSecretNotFoundErrorType, Path: path, Key: key}
			}
		} else {
			for _, w := range warnings {
				logger.Warningln(w)
			}
			metrics.updateVaultSecretReadErrorsCountMetric(path, key, errors.BackendSecretNotFoundErrorType)
			err = &errors.BackendSecretNotFoundError{ErrType: errors.BackendSecretNotFoundErrorType, Path: path, Key: key}
		}
	} else {
		metrics.updateVaultSecretReadErrorsCountMetric(path, key, errors.BackendSecretNotFoundErrorType)
		err = &errors.BackendSecretNotFoundError{ErrType: errors.BackendSecretNotFoundErrorType, Path: path, Key: key}
	}
	return data, err
}
