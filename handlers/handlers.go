package handlers

import (
	"fmt"
	"html/template"
	"net/http"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/vouch/vouch-proxy/handlers/adfs"
	"github.com/vouch/vouch-proxy/handlers/common"
	"github.com/vouch/vouch-proxy/handlers/github"
	"github.com/vouch/vouch-proxy/handlers/google"
	"github.com/vouch/vouch-proxy/handlers/homeassistant"
	"github.com/vouch/vouch-proxy/handlers/indieauth"
	"github.com/vouch/vouch-proxy/handlers/nextcloud"
	"github.com/vouch/vouch-proxy/handlers/openid"
	"github.com/vouch/vouch-proxy/handlers/openstax"

	"go.uber.org/zap"

	securerandom "github.com/theckman/go-securerandom"

	"github.com/gorilla/sessions"
	"github.com/vouch/vouch-proxy/pkg/cfg"
	"github.com/vouch/vouch-proxy/pkg/cookie"
	"github.com/vouch/vouch-proxy/pkg/domains"
	"github.com/vouch/vouch-proxy/pkg/jwtmanager"
	"github.com/vouch/vouch-proxy/pkg/structs"
	"golang.org/x/oauth2"
)

// Index variables passed to index.tmpl
type Index struct {
	Msg      string
	TestURLs []string
	Testing  bool
}

// AuthError sets the values to return to nginx
type AuthError struct {
	Error string
	JWT   string
}

// Provider each Provider must support GetuserInfo
type Provider interface {
	Configure()
	GetUserInfo(r *http.Request, user *structs.User, customClaims *structs.CustomClaims, ptokens *structs.PTokens) error
}

const (
	base64Bytes = 32
)

var (
	indexTemplate *template.Template
	sessstore     *sessions.CookieStore
	log           *zap.SugaredLogger
	fastlog       *zap.Logger
	provider      Provider
)

// Configure see main.go configure()
func Configure() {
	log = cfg.Logging.Logger
	fastlog = cfg.Logging.FastLogger
	// http://www.gorillatoolkit.org/pkg/sessions
	sessstore = sessions.NewCookieStore([]byte(cfg.Cfg.Session.Key))
	sessstore.Options.HttpOnly = cfg.Cfg.Cookie.HTTPOnly
	sessstore.Options.Secure = cfg.Cfg.Cookie.Secure

	log.Debugf("handlers.Configure() attempting to parse templates with cfg.RootDir: %s", cfg.RootDir)
	indexTemplate = template.Must(template.ParseFiles(filepath.Join(cfg.RootDir, "templates/index.tmpl")))

	provider = getProvider()
	provider.Configure()
}

func loginURL(r *http.Request, state string) string {
	// State can be some kind of random generated hash string.
	// See relevant RFC: http://tools.ietf.org/html/rfc6749#section-10.12
	var lurl = ""
	if cfg.GenOAuth.Provider == cfg.Providers.IndieAuth {
		lurl = cfg.OAuthClient.AuthCodeURL(state, oauth2.SetAuthURLParam("response_type", "id"))
	} else if cfg.GenOAuth.Provider == cfg.Providers.ADFS {
		lurl = cfg.OAuthClient.AuthCodeURL(state, cfg.OAuthopts)
	} else {
		domain := domains.Matches(r.Host)
		log.Debugf("looking for redirect URL matching  %v", domain)
		for i, v := range cfg.GenOAuth.RedirectURLs {
			if strings.Contains(v, domain) {
				log.Debugf("redirect value matched at [%d]=%v", i, v)
				cfg.OAuthClient.RedirectURL = v
				break
			}
		}
		if cfg.OAuthopts != nil {
			lurl = cfg.OAuthClient.AuthCodeURL(state, cfg.OAuthopts)
		} else {
			lurl = cfg.OAuthClient.AuthCodeURL(state)
		}
	}
	// log.Debugf("loginURL %s", url)
	return lurl
}

// FindJWT look for JWT in Cookie, JWT Header, Authorization Header (OAuth2 Bearer Token)
// and Query String in that order
func FindJWT(r *http.Request) string {
	jwt, err := cookie.Cookie(r)
	if err == nil {
		log.Debugf("jwt from cookie: %s", jwt)
		return jwt
	}
	jwt = r.Header.Get(cfg.Cfg.Headers.JWT)
	if jwt != "" {
		log.Debugf("jwt from header %s: %s", cfg.Cfg.Headers.JWT, jwt)
		return jwt
	}
	auth := r.Header.Get("Authorization")
	if auth != "" {
		s := strings.SplitN(auth, " ", 2)
		if len(s) == 2 {
			jwt = s[1]
			log.Debugf("jwt from authorization header: %s", jwt)
			return jwt
		}
	}
	jwt = r.URL.Query().Get(cfg.Cfg.Headers.QueryString)
	if jwt != "" {
		log.Debugf("jwt from querystring %s: %s", cfg.Cfg.Headers.QueryString, jwt)
		return jwt
	}
	return ""
}

// ClaimsFromJWT parse the jwt and return the claims
func ClaimsFromJWT(jwt string) (jwtmanager.VouchClaims, error) {
	var claims jwtmanager.VouchClaims

	jwtParsed, err := jwtmanager.ParseTokenString(jwt)
	if err != nil {
		// it didn't parse, which means its bad, start over
		log.Error("jwtParsed returned error, clearing cookie")
		return claims, err
	}

	claims, err = jwtmanager.PTokenClaims(jwtParsed)
	if err != nil {
		// claims = jwtmanager.PTokenClaims(jwtParsed)
		// if claims == &jwtmanager.VouchClaims{} {
		return claims, err
	}
	log.Debugf("JWT Claims: %+v", claims)
	return claims, nil
}

// ValidateRequestHandler /validate
// TODO this should use the handler interface
func ValidateRequestHandler(w http.ResponseWriter, r *http.Request) {
	fastlog.Debug("/validate")

	// TODO: collapse all of the `if !cfg.Cfg.PublicAccess` calls
	// perhaps using an `ok=false` pattern
	jwt := FindJWT(r)
	// if jwt != "" {
	if jwt == "" {
		// If the module is configured to allow public access with no authentication, return 200 now
		if cfg.Cfg.PublicAccess {
			w.Header().Add(cfg.Cfg.Headers.User, "")
			log.Debugf("no jwt found, but public access is '%v', returning ok200", cfg.Cfg.PublicAccess)
			ok200(w, r)
		} else {
			error401(w, r, AuthError{Error: "no jwt found in request"})
		}
		return
	}

	claims, err := ClaimsFromJWT(jwt)
	if err != nil {
		// no email in jwt
		if !cfg.Cfg.PublicAccess {
			error401(w, r, AuthError{err.Error(), jwt})
		} else {
			w.Header().Add(cfg.Cfg.Headers.User, "")
		}
		return
	}

	if claims.Username == "" {
		// no email in jwt
		if !cfg.Cfg.PublicAccess {
			error401(w, r, AuthError{"no Username found in jwt", jwt})
		} else {
			w.Header().Add(cfg.Cfg.Headers.User, "")
		}
		return
	}
	fastlog.Info("jwt cookie",
		zap.String("username", claims.Username))

	if !cfg.Cfg.AllowAllUsers {
		if !jwtmanager.SiteInClaims(r.Host, &claims) {
			if !cfg.Cfg.PublicAccess {
				error401(w, r, AuthError{
					fmt.Sprintf("http header 'Host: %s' not authorized for configured `vouch.domains` (is Host being sent properly?)", r.Host),
					jwt})
			} else {
				w.Header().Add(cfg.Cfg.Headers.User, "")
			}
			return
		}
	}

	if len(cfg.Cfg.Headers.ClaimsCleaned) > 0 {
		log.Debug("Found claims in config, finding specific keys...")
		// Run through all the claims found
		for k, v := range claims.CustomClaims {
			// Run through the claims we are looking for
			for claim, header := range cfg.Cfg.Headers.ClaimsCleaned {
				// Check for matching claim
				if claim == k {
					log.Debug("Found matching claim key: ", k)
					// <<<<<<< HEAD
					// customHeader := strings.Join([]string{cfg.Cfg.Headers.ClaimHeader, k}, "")
					if val, ok := v.([]interface{}); ok {
						// =======
						// 					// convert to string
						// 					val := fmt.Sprint(v)
						// 					if reflect.TypeOf(val).Kind() == reflect.String {
						// 						// if val, ok := v.(string); ok {
						// 						log.Debugf("Adding header for claim %s - %s: %s", k, header, val)
						// 						w.Header().Add(header, val)
						// 					} else if val, ok := v.([]interface{}); ok {
						// >>>>>>> master
						strs := make([]string, len(val))
						for i, v := range val {
							strs[i] = fmt.Sprintf("\"%s\"", v)
						}
						log.Debugf("Adding header for claim %s - %s: %s", k, header, val)
						w.Header().Add(header, strings.Join(strs, ","))
					} else {
						// convert to string
						val := fmt.Sprint(v)
						if reflect.TypeOf(val).Kind() == reflect.String {
							// if val, ok := v.(string); ok {
							w.Header().Add(header, val)
							log.Debugf("Adding header for claim %s - %s: %s", k, header, val)
						} else {
							log.Errorf("Couldn't parse header type for %s %+v.  Please submit an issue.", k, v)
						}
					}
				}
			}
		}
	}

	w.Header().Add(cfg.Cfg.Headers.User, claims.Username)
	w.Header().Add(cfg.Cfg.Headers.Success, "true")

	if cfg.Cfg.Headers.AccessToken != "" {
		if claims.PAccessToken != "" {
			w.Header().Add(cfg.Cfg.Headers.AccessToken, claims.PAccessToken)
		}
	}
	if cfg.Cfg.Headers.IDToken != "" {
		if claims.PIdToken != "" {
			w.Header().Add(cfg.Cfg.Headers.IDToken, claims.PIdToken)

		}
	}
	// fastlog.Debugf("response headers %+v", w.Header())
	// fastlog.Debug("response header",
	// 	zap.String(cfg.Cfg.Headers.User, w.Header().Get(cfg.Cfg.Headers.User)))
	fastlog.Debug("response header",
		zap.Any("all headers", w.Header()))

	// good to go!!
	if cfg.Cfg.Testing {
		renderIndex(w, "user authorized "+claims.Username)
	} else {
		ok200(w, r)
	}

	// TODO
	// parse the jwt and see if the claim is valid for the domain

}

// LogoutHandler /logout
// currently performs a 302 redirect to Google
func LogoutHandler(w http.ResponseWriter, r *http.Request) {
	log.Debug("/logout")
	cookie.ClearCookie(w, r)

	log.Debug("deleting session")
	sessstore.MaxAge(-1)
	session, err := sessstore.Get(r, cfg.Cfg.Session.Name)
	if err != nil {
		log.Error(err)
	}
	if err = session.Save(r, w); err != nil {
		log.Error(err)
	}
	sessstore.MaxAge(300)

	var requestedURL = r.URL.Query().Get("url")
	if requestedURL != "" {
		redirect302(w, r, requestedURL)
	} else {
		renderIndex(w, "/logout you have been logged out")
	}
}

// HealthcheckHandler /healthcheck
// just returns 200 '{ "ok": true }'
func HealthcheckHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprintf(w, "{ \"ok\": true }"); err != nil {
		log.Error(err)
	}
}

var regExJustAlphaNum, _ = regexp.Compile("[^a-zA-Z0-9]+")

func generateStateNonce() (string, error) {
	state, err := securerandom.URLBase64InBytes(base64Bytes)
	if err != nil {
		return "", err
	}
	state = regExJustAlphaNum.ReplaceAllString(state, "")
	return state, nil
}

// LoginHandler /login
// currently performs a 302 redirect to Google
func LoginHandler(w http.ResponseWriter, r *http.Request) {
	log.Debug("/login")
	// no matter how you ended up here, make sure the cookie gets cleared out
	cookie.ClearCookie(w, r)

	session, err := sessstore.Get(r, cfg.Cfg.Session.Name)
	if err != nil {
		log.Warnf("couldn't find existing encrypted secure cookie with name %s: %s (probably fine)", cfg.Cfg.Session.Name, err)
	}

	state, err := generateStateNonce()
	if err != nil {
		log.Error(err)
	}

	// set the state variable in the session
	session.Values["state"] = state
	log.Debugf("session state set to %s", session.Values["state"])

	// increment the failure counter for this domain

	// requestedURL comes from nginx in the query string via a 302 redirect
	// it sets the ultimate destination
	// https://vouch.yoursite.com/login?url=
	var requestedURL = r.URL.Query().Get("url")
	if requestedURL == "" {
		renderIndex(w, "/login no destination URL requested")
		log.Error("no destination URL requested")
		return
	}

	// set session variable for eventual 302 redirecton to original request
	session.Values["requestedURL"] = requestedURL
	log.Debugf("session requestedURL set to %s", session.Values["requestedURL"])

	// stop them after three failures for this URL
	var failcount = 0
	if session.Values[requestedURL] != nil {
		failcount = session.Values[requestedURL].(int)
		log.Debugf("failcount for %s is %d", requestedURL, failcount)
	}
	failcount++
	session.Values[requestedURL] = failcount

	log.Debug("saving session")
	if err = session.Save(r, w); err != nil {
		log.Error(err)
	}

	if failcount > 2 {
		var vouchError = r.URL.Query().Get("error")
		renderIndex(w, "/login too many redirects for "+requestedURL+" - "+vouchError)
	} else {
		// bounce to oauth provider for login
		var lURL = loginURL(r, state)
		log.Debugf("redirecting to oauthURL %s", lURL)
		redirect302(w, r, lURL)
	}
}

func renderIndex(w http.ResponseWriter, msg string) {
	if err := indexTemplate.Execute(w, &Index{Msg: msg, TestURLs: cfg.Cfg.TestURLs, Testing: cfg.Cfg.Testing}); err != nil {
		log.Error(err)
	}
}

// VerifyUser validates that the domains match for the user
func VerifyUser(u interface{}) (bool, error) {

	user := u.(structs.User)

	switch {

	// AllowAllUsers
	case cfg.Cfg.AllowAllUsers:
		log.Debugf("VerifyUser: Success! skipping verification, cfg.Cfg.AllowAllUsers is %t", cfg.Cfg.AllowAllUsers)
		return true, nil

	// WhiteList
	case len(cfg.Cfg.WhiteList) != 0:
		for _, wl := range cfg.Cfg.WhiteList {
			if user.Username == wl {
				log.Debugf("VerifyUser: Success! found user.Username in WhiteList: %s", user.Username)
				return true, nil
			}
		}
		return false, fmt.Errorf("VerifyUser: user.Username not found in WhiteList: %s", user.Username)

	// TeamWhiteList
	case len(cfg.Cfg.TeamWhiteList) != 0:
		for _, team := range user.TeamMemberships {
			for _, wl := range cfg.Cfg.TeamWhiteList {
				if team == wl {
					log.Debugf("VerifyUser: Success! found user.TeamWhiteList in TeamWhiteList: %s for user %s", wl, user.Username)
					return true, nil
				}
			}
		}
		return false, fmt.Errorf("VerifyUser: user.TeamMemberships %s not found in TeamWhiteList: %s for user %s", user.TeamMemberships, cfg.Cfg.TeamWhiteList, user.Username)

	// Domains
	case len(cfg.Cfg.Domains) != 0:
		if domains.IsUnderManagement(user.Email) {
			log.Debugf("VerifyUser: Success! Email %s found within a "+cfg.Branding.CcName+" managed domain", user.Email)
			return true, nil
		}
		return false, fmt.Errorf("VerifyUser: Email %s is not within a "+cfg.Branding.CcName+" managed domain", user.Email)

	// nothing configured, allow everyone through
	default:
		log.Warn("VerifyUser: no domains, whitelist, teamWhitelist or AllowAllUsers configured, any successful auth to the IdP authorizes access")
		return true, nil
	}
}

// CallbackHandler /auth
// - validate info from oauth provider (Google, GitHub, OIDC, etc)
// - create user
// - issue jwt in the form of a cookie
func CallbackHandler(w http.ResponseWriter, r *http.Request) {
	log.Debug("/auth")
	// Handle the exchange code to initiate a transport.

	session, err := sessstore.Get(r, cfg.Cfg.Session.Name)
	if err != nil {
		log.Errorf("/auth could not find session store %s", cfg.Cfg.Session.Name)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// is the nonce "state" valid?
	queryState := r.URL.Query().Get("state")
	if session.Values["state"] != queryState {
		log.Errorf("/auth Invalid session state: stored %s, returned %s", session.Values["state"], queryState)
		renderIndex(w, "/auth Invalid session state.")
		return
	}

	errorState := r.URL.Query().Get("error")
	if errorState != "" {
		errorDescription := r.URL.Query().Get("error_description")
		log.Warn("/auth Error state: ", errorState, ", Error description: ", errorDescription)
		w.WriteHeader(http.StatusForbidden)
		renderIndex(w, "FORBIDDEN: "+errorDescription)
		return
	}

	user := structs.User{}
	customClaims := structs.CustomClaims{}
	ptokens := structs.PTokens{}

	if err := getUserInfo(r, &user, &customClaims, &ptokens); err != nil {
		log.Error(err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Debugf("/auth Claims from userinfo: %+v", customClaims)
	//getProviderJWT(r, &user)
	log.Debug("/auth CallbackHandler")
	log.Debugf("/auth %+v", user)
	log.Debugf("requestedURL %v", session.Values["requestedURL"].(string))

	if ok, err := VerifyUser(user); !ok {
		log.Error(err)
//		renderIndex(w, fmt.Sprintf("/auth User is not authorized. %s Please try again.", err))
                requestedURL := session.Values["requestedURL"].(string)
                if requestedURL != "" {
	                log.Debugf("requestedURL = $v", requestedURL)
                        renderIndex(w, fmt.Sprintf("/auth User is not authorized. %s <a href=\"%s\">Please try again</a>.", err, requestedURL))
                } else {
                        renderIndex(w, fmt.Sprintf("/auth User is not authorized. %s Please try again.", err))
                }
		return
	}

	// SUCCESS!! they are authorized

	// issue the jwt
	tokenstring := jwtmanager.CreateUserTokenString(user, customClaims, ptokens)
	cookie.SetCookie(w, r, tokenstring)

	// get the originally requested URL so we can send them on their way
	requestedURL := session.Values["requestedURL"].(string)
	if requestedURL != "" {
		// clear out the session value
		session.Values["requestedURL"] = ""
		session.Values[requestedURL] = 0
		if err = session.Save(r, w); err != nil {
			log.Error(err)
		}

		redirect302(w, r, requestedURL)
		return
	}
	// otherwise serve an html page
	renderIndex(w, "/auth "+tokenstring)
}

func getUserInfo(r *http.Request, user *structs.User, customClaims *structs.CustomClaims, ptokens *structs.PTokens) error {
	return provider.GetUserInfo(r, user, customClaims, ptokens)
}

func getProvider() Provider {
	switch cfg.GenOAuth.Provider {
	case cfg.Providers.IndieAuth:
		return indieauth.Provider{}
	case cfg.Providers.ADFS:
		return adfs.Provider{}
	case cfg.Providers.HomeAssistant:
		return homeassistant.Provider{}
	case cfg.Providers.OpenStax:
		return openstax.Provider{}
	case cfg.Providers.Google:
		return google.Provider{}
	case cfg.Providers.GitHub:
		return github.Provider{PrepareTokensAndClient: common.PrepareTokensAndClient}
	case cfg.Providers.Nextcloud:
		return nextcloud.Provider{}
	case cfg.Providers.OIDC:
		return openid.Provider{}
	default:
		// shouldn't ever reach this since cfg checks for a properly configure `oauth.provider`
		log.Fatal("oauth.provider appears to be misconfigured, please check your config")
		return nil
	}
}

// the standard error
// this is captured by nginx, which converts the 401 into 302 to the login page
func error401(w http.ResponseWriter, r *http.Request, ae AuthError) {
	log.Error(ae.Error)
	cookie.ClearCookie(w, r)
	// w.Header().Set("X-Vouch-Error", ae.Error)
	http.Error(w, ae.Error, http.StatusUnauthorized)
	// TODO put this back in place if multiple auth mechanism are available
	// c.HTML(http.StatusBadRequest, "error.tmpl", gin.H{"message": errStr})
}

func error401na(w http.ResponseWriter, r *http.Request) {
	error401(w, r, AuthError{Error: "not authorized"})
}

func redirect302(w http.ResponseWriter, r *http.Request, rURL string) {
	if cfg.Cfg.Testing {
		cfg.Cfg.TestURLs = append(cfg.Cfg.TestURLs, rURL)
		renderIndex(w, "302 redirect to: "+rURL)
		return
	}
	http.Redirect(w, r, rURL, http.StatusFound)
}

func ok200(w http.ResponseWriter, r *http.Request) {
	_, err := w.Write([]byte("200 OK\n"))
	if err != nil {
		log.Error(err)
	}
}
