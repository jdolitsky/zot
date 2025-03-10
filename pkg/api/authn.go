package api

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chartmuseum/auth"
	"github.com/gorilla/mux"
	"golang.org/x/crypto/bcrypt"

	"zotregistry.io/zot/errors"
	"zotregistry.io/zot/pkg/api/config"
	"zotregistry.io/zot/pkg/api/constants"
	apiErr "zotregistry.io/zot/pkg/api/errors"
	"zotregistry.io/zot/pkg/common"
	localCtx "zotregistry.io/zot/pkg/requestcontext"
)

const (
	bearerAuthDefaultAccessEntryType = "repository"
)

func AuthHandler(c *Controller) mux.MiddlewareFunc {
	if isBearerAuthEnabled(c.Config) {
		return bearerAuthHandler(c)
	}

	return basicAuthHandler(c)
}

func bearerAuthHandler(ctlr *Controller) mux.MiddlewareFunc {
	authorizer, err := auth.NewAuthorizer(&auth.AuthorizerOptions{
		Realm:                 ctlr.Config.HTTP.Auth.Bearer.Realm,
		Service:               ctlr.Config.HTTP.Auth.Bearer.Service,
		PublicKeyPath:         ctlr.Config.HTTP.Auth.Bearer.Cert,
		AccessEntryType:       bearerAuthDefaultAccessEntryType,
		EmptyDefaultNamespace: true,
	})
	if err != nil {
		ctlr.Log.Panic().Err(err).Msg("error creating bearer authorizer")
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			if request.Method == http.MethodOptions {
				next.ServeHTTP(response, request)
				response.WriteHeader(http.StatusNoContent)

				return
			}
			vars := mux.Vars(request)
			name := vars["name"]

			// we want to bypass auth for mgmt route
			isMgmtRequested := request.RequestURI == constants.FullMgmtPrefix

			header := request.Header.Get("Authorization")

			if (header == "" || header == "Basic Og==") && isMgmtRequested {
				next.ServeHTTP(response, request)

				return
			}

			action := auth.PullAction
			if m := request.Method; m != http.MethodGet && m != http.MethodHead {
				action = auth.PushAction
			}
			permissions, err := authorizer.Authorize(header, action, name)
			if err != nil {
				ctlr.Log.Error().Err(err).Msg("issue parsing Authorization header")
				response.Header().Set("Content-Type", "application/json")
				common.WriteJSON(response, http.StatusInternalServerError, apiErr.NewErrorList(apiErr.NewError(apiErr.UNSUPPORTED)))

				return
			}

			if !permissions.Allowed {
				authFail(response, permissions.WWWAuthenticateHeader, 0)

				return
			}

			next.ServeHTTP(response, request)
		})
	}
}

func noPasswdAuth(realm string, config *config.Config) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			if request.Method == http.MethodOptions {
				next.ServeHTTP(response, request)
				response.WriteHeader(http.StatusNoContent)

				return
			}

			// Process request
			ctx := getReqContextWithAuthorization("", []string{}, request)
			next.ServeHTTP(response, request.WithContext(ctx)) //nolint:contextcheck
		})
	}
}

//nolint:gocyclo  // we use closure making this a complex subroutine
func basicAuthHandler(ctlr *Controller) mux.MiddlewareFunc {
	realm := ctlr.Config.HTTP.Realm
	if realm == "" {
		realm = "Authorization Required"
	}

	realm = "Basic realm=" + strconv.Quote(realm)

	// no password based authN, if neither LDAP nor HTTP BASIC is enabled
	if ctlr.Config.HTTP.Auth == nil ||
		(ctlr.Config.HTTP.Auth.HTPasswd.Path == "" && ctlr.Config.HTTP.Auth.LDAP == nil) {
		return noPasswdAuth(realm, ctlr.Config)
	}

	credMap := make(map[string]string)

	delay := ctlr.Config.HTTP.Auth.FailDelay

	var ldapClient *LDAPClient

	if ctlr.Config.HTTP.Auth != nil {
		if ctlr.Config.HTTP.Auth.LDAP != nil {
			ldapConfig := ctlr.Config.HTTP.Auth.LDAP
			ldapClient = &LDAPClient{
				Host:               ldapConfig.Address,
				Port:               ldapConfig.Port,
				UseSSL:             !ldapConfig.Insecure,
				SkipTLS:            !ldapConfig.StartTLS,
				Base:               ldapConfig.BaseDN,
				BindDN:             ldapConfig.BindDN,
				UserGroupAttribute: ldapConfig.UserGroupAttribute, // from config
				BindPassword:       ldapConfig.BindPassword,
				UserFilter:         fmt.Sprintf("(%s=%%s)", ldapConfig.UserAttribute),
				InsecureSkipVerify: ldapConfig.SkipVerify,
				ServerName:         ldapConfig.Address,
				Log:                ctlr.Log,
				SubtreeSearch:      ldapConfig.SubtreeSearch,
			}

			if ctlr.Config.HTTP.Auth.LDAP.CACert != "" {
				caCert, err := os.ReadFile(ctlr.Config.HTTP.Auth.LDAP.CACert)
				if err != nil {
					panic(err)
				}

				caCertPool := x509.NewCertPool()

				if !caCertPool.AppendCertsFromPEM(caCert) {
					panic(errors.ErrBadCACert)
				}

				ldapClient.ClientCAs = caCertPool
			} else {
				// default to system cert pool
				caCertPool, err := x509.SystemCertPool()
				if err != nil {
					panic(errors.ErrBadCACert)
				}

				ldapClient.ClientCAs = caCertPool
			}
		}

		if ctlr.Config.HTTP.Auth.HTPasswd.Path != "" {
			credsFile, err := os.Open(ctlr.Config.HTTP.Auth.HTPasswd.Path)
			if err != nil {
				panic(err)
			}
			defer credsFile.Close()

			scanner := bufio.NewScanner(credsFile)

			for scanner.Scan() {
				line := scanner.Text()
				if strings.Contains(line, ":") {
					tokens := strings.Split(scanner.Text(), ":")
					credMap[tokens[0]] = tokens[1]
				}
			}
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			if request.Method == http.MethodOptions {
				next.ServeHTTP(response, request)
				response.WriteHeader(http.StatusNoContent)

				return
			}

			// we want to bypass auth for mgmt route
			isMgmtRequested := request.RequestURI == constants.FullMgmtPrefix

			if request.Header.Get("Authorization") == "" {
				if ctlr.Config.HTTP.AccessControl.AnonymousPolicyExists() || isMgmtRequested {
					// Process request
					ctx := getReqContextWithAuthorization("", []string{}, request)
					next.ServeHTTP(response, request.WithContext(ctx)) //nolint:contextcheck

					return
				}
			}

			username, passphrase, err := getUsernamePasswordBasicAuth(request)
			if err != nil {
				ctlr.Log.Error().Err(err).Msg("failed to parse authorization header")
				authFail(response, realm, delay)

				return
			}

			// some client tools might send Authorization: Basic Og== (decoded into ":")
			// empty username and password
			if username == "" && passphrase == "" {
				if ctlr.Config.HTTP.AccessControl.AnonymousPolicyExists() || isMgmtRequested {
					// Process request
					ctx := getReqContextWithAuthorization("", []string{}, request)
					next.ServeHTTP(response, request.WithContext(ctx)) //nolint:contextcheck

					return
				}
			}

			// first, HTTPPassword authN (which is local)
			passphraseHash, ok := credMap[username]
			if ok {
				if err := bcrypt.CompareHashAndPassword([]byte(passphraseHash), []byte(passphrase)); err == nil {
					// Process request
					var userGroups []string

					if ctlr.Config.HTTP.AccessControl != nil {
						ac := NewAccessController(ctlr.Config)
						userGroups = ac.getUserGroups(username)
					}

					ctx := getReqContextWithAuthorization(username, userGroups, request)
					next.ServeHTTP(response, request.WithContext(ctx)) //nolint:contextcheck

					return
				}
			}

			// next, LDAP if configured (network-based which can lose connectivity)
			if ctlr.Config.HTTP.Auth != nil && ctlr.Config.HTTP.Auth.LDAP != nil {
				ok, _, ldapgroups, err := ldapClient.Authenticate(username, passphrase)
				if ok && err == nil {
					// Process request
					var userGroups []string

					if ctlr.Config.HTTP.AccessControl != nil {
						ac := NewAccessController(ctlr.Config)
						userGroups = ac.getUserGroups(username)
					}

					userGroups = append(userGroups, ldapgroups...)

					ctx := getReqContextWithAuthorization(username, userGroups, request)
					next.ServeHTTP(response, request.WithContext(ctx)) //nolint:contextcheck

					return
				}
			}

			authFail(response, realm, delay)
		})
	}
}

func getReqContextWithAuthorization(username string, groups []string, request *http.Request) context.Context {
	acCtx := localCtx.AccessControlContext{
		Username: username,
		Groups:   groups,
	}

	authzCtxKey := localCtx.GetContextKey()
	ctx := context.WithValue(request.Context(), authzCtxKey, acCtx)

	return ctx
}

func isAuthnEnabled(config *config.Config) bool {
	if config.HTTP.Auth != nil &&
		(config.HTTP.Auth.HTPasswd.Path != "" || config.HTTP.Auth.LDAP != nil) {
		return true
	}

	return false
}

func isBearerAuthEnabled(config *config.Config) bool {
	if config.HTTP.Auth != nil &&
		config.HTTP.Auth.Bearer != nil &&
		config.HTTP.Auth.Bearer.Cert != "" &&
		config.HTTP.Auth.Bearer.Realm != "" &&
		config.HTTP.Auth.Bearer.Service != "" {
		return true
	}

	return false
}

func authFail(w http.ResponseWriter, realm string, delay int) {
	time.Sleep(time.Duration(delay) * time.Second)
	w.Header().Set("WWW-Authenticate", realm)
	w.Header().Set("Content-Type", "application/json")
	common.WriteJSON(w, http.StatusUnauthorized, apiErr.NewErrorList(apiErr.NewError(apiErr.UNAUTHORIZED)))
}

func getUsernamePasswordBasicAuth(request *http.Request) (string, string, error) {
	basicAuth := request.Header.Get("Authorization")

	if basicAuth == "" {
		return "", "", errors.ErrParsingAuthHeader
	}

	splitStr := strings.SplitN(basicAuth, " ", 2) //nolint:gomnd
	if len(splitStr) != 2 || strings.ToLower(splitStr[0]) != "basic" {
		return "", "", errors.ErrParsingAuthHeader
	}

	decodedStr, err := base64.StdEncoding.DecodeString(splitStr[1])
	if err != nil {
		return "", "", err
	}

	pair := strings.SplitN(string(decodedStr), ":", 2) //nolint:gomnd
	if len(pair) != 2 {                                //nolint:gomnd
		return "", "", errors.ErrParsingAuthHeader
	}

	username := pair[0]
	passphrase := pair[1]

	return username, passphrase, nil
}
