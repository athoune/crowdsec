package v1

import (
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/crowdsecurity/crowdsec/pkg/database"
	"github.com/crowdsecurity/crowdsec/pkg/database/ent"
	"github.com/crowdsecurity/crowdsec/pkg/types"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

var (
	APIKeyHeader = "X-Api-Key"
)

type APIKey struct {
	HeaderName string
	DbClient   *database.Client
	TlsAuth    *TLSAuth
}

func GenerateAPIKey(n int) (string, error) {
	bytes := make([]byte, n)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func NewAPIKey(dbClient *database.Client) *APIKey {
	return &APIKey{
		HeaderName: APIKeyHeader,
		DbClient:   dbClient,
		TlsAuth:    &TLSAuth{},
	}
}

func HashSHA512(str string) string {
	hashedKey := sha512.New()
	hashedKey.Write([]byte(str))

	hashStr := fmt.Sprintf("%x", hashedKey.Sum(nil))

	return hashStr
}

func (a *APIKey) MiddlewareFunc() gin.HandlerFunc {
	return func(c *gin.Context) {
		var bouncer *ent.Bouncer
		var err error

		if c.Request.TLS != nil && len(c.Request.TLS.PeerCertificates) > 0 {
			validCert, extractedCN, err := a.TlsAuth.ValidateCert(c)
			if !validCert {
				c.JSON(http.StatusForbidden, gin.H{"message": "access forbidden"})
				c.Abort()
				return
			}
			if err != nil {
				log.Error(err)
				c.JSON(http.StatusForbidden, gin.H{"message": "access forbidden"})
				c.Abort()
				return
			}
			bouncerName := fmt.Sprintf("%s@%s", extractedCN, c.ClientIP())
			log.Debugf("Trying to find bouncer %s for cert", bouncerName)
			bouncer, err = a.DbClient.SelectBouncerByName(bouncerName)
			if ent.IsNotFound(err) {
				//Because we have a valid cert, automatically create the bouncer in the database if it does not exist
				//Set a random API key, but it will never be used
				//To fix
				apiKey, err := GenerateAPIKey(64)
				if err != nil {
					log.Errorf("auth api key error: %s", err)
					c.JSON(http.StatusForbidden, gin.H{"message": "access forbidden"})
					c.Abort()
					return
				}
				log.Infof("Creating bouncer %s", bouncerName)
				err = a.DbClient.CreateBouncer(bouncerName, c.ClientIP(), HashSHA512(apiKey), types.TlsAuthType)
				if err != nil {
					log.Errorf("auth api key error: %s", err)
					c.JSON(http.StatusForbidden, gin.H{"message": "access forbidden"})
					c.Abort()
					return
				}
				log.Info("Getting bouncer %s from db after creation", bouncerName)
				bouncer, err = a.DbClient.SelectBouncerByName(bouncerName)
				if err != nil {
					log.Errorf("auth api key error: %s", err)
					c.JSON(http.StatusForbidden, gin.H{"message": "access forbidden"})
					c.Abort()
					return
				}
				log.Infof("Got bouncer %s", bouncer.Name)
				log.Infof("Bouncer details: %+v", bouncer)
			} else if err != nil {
				log.Errorf("auth api key error: %s", err)
				c.JSON(http.StatusForbidden, gin.H{"message": "access forbidden"})
				c.Abort()
				return
			} else {
				if bouncer.AuthType != types.TlsAuthType {
					log.Errorf("auth api key error: %s", err)
					c.JSON(http.StatusForbidden, gin.H{"message": "access forbidden"})
					c.Abort()
					return
				}
			}
		} else {
			val, ok := c.Request.Header[APIKeyHeader]
			if !ok {
				c.JSON(http.StatusForbidden, gin.H{"message": "access forbidden"})
				c.Abort()
				return
			}
			hashStr := HashSHA512(val[0])
			bouncer, err = a.DbClient.SelectBouncer(hashStr)
			if err != nil {
				log.Errorf("auth api key error: %s", err)
				c.JSON(http.StatusForbidden, gin.H{"message": "access forbidden"})
				c.Abort()
				return
			}
			if bouncer.AuthType != types.ApiKeyAuthType {
				log.Errorf("bouncer %s attempted to login using an API key but it is configured to auth with TLS cert", bouncer.Name)
				c.JSON(http.StatusForbidden, gin.H{"message": "access forbidden"})
				c.Abort()
				return
			}
		}

		if bouncer == nil {
			c.JSON(http.StatusForbidden, gin.H{"message": "access forbidden"})
			c.Abort()
			return
		}

		c.Set("BOUNCER_NAME", bouncer.Name)

		if bouncer.IPAddress == "" {
			err = a.DbClient.UpdateBouncerIP(c.ClientIP(), bouncer.ID)
			if err != nil {
				log.Errorf("Failed to update ip address for '%s': %s\n", bouncer.Name, err)
				c.JSON(http.StatusForbidden, gin.H{"message": "access forbidden"})
				c.Abort()
				return
			}
		}

		if bouncer.IPAddress != c.ClientIP() && bouncer.IPAddress != "" {
			log.Warningf("new IP address detected for bouncer '%s': %s (old: %s)", bouncer.Name, c.ClientIP(), bouncer.IPAddress)
			err = a.DbClient.UpdateBouncerIP(c.ClientIP(), bouncer.ID)
			if err != nil {
				log.Errorf("Failed to update ip address for '%s': %s\n", bouncer.Name, err)
				c.JSON(http.StatusForbidden, gin.H{"message": "access forbidden"})
				c.Abort()
				return
			}
		}

		useragent := strings.Split(c.Request.UserAgent(), "/")

		if len(useragent) != 2 {
			log.Warningf("bad user agent '%s' from '%s'", c.Request.UserAgent(), c.ClientIP())
			useragent = []string{c.Request.UserAgent(), "N/A"}
		}

		if bouncer.Version != useragent[1] || bouncer.Type != useragent[0] {
			if err := a.DbClient.UpdateBouncerTypeAndVersion(useragent[0], useragent[1], bouncer.ID); err != nil {
				log.Errorf("failed to update bouncer version and type from '%s' (%s): %s", c.Request.UserAgent(), c.ClientIP(), err)
				c.JSON(http.StatusForbidden, gin.H{"message": "bad user agent"})
				c.Abort()
				return
			}
		}
		c.Next()
	}
}
