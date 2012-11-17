package auth

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	//"strings"
	"time"

	"code.google.com/p/vitess/go/cache"
)

// cache used to store the oauth_token_secret between sessions. By default it
// stores 1MB of data. When the limit is reached the cache will clear out older
// items (which by the time they are removed from the cache should not be
// needed anymore). 
var tokenCache = cache.NewLRUCache(1048576) 

// tokenCacheItem represents an OAuth token that implements the cache.Value
// interface, and can therefore be stored in the LRUCache.
//type tokenCacheItem string;
//func (i tokenCacheItem) Size()   int    { return len(i) }
//func (i tokenCacheItem) String() string { return string(i) }



// requestToken stores the values returned when requesting a request token. The
// request token is used to obtain authorization from a user, and exchanged
// for an access token.
type requestToken {
	Token  string // the oauth_token value
	Secret string // the oauth_token_secret value
}

// Gets the size (in bytes) of the Token. This is used to implement the
// cache.Value interface, allowing this struct to be stored in the LRUCache.
func (t requestToken) Size() int {
	return len(t.Token) + len(t.Secret)
}

// accessToken stores the values returned when upgrading a request token
// to an access token. The access token gives the consumer access to the
// User's protected resources.
type accessToken {
	Token  string // the oauth_token value
	Secret string // the oauth_token_secret value
}

// Abstract implementation of OAuth2 for user authentication.
type OAuth1Mixin struct {
	AuthorizeUrl    string
	RequestToken    string
	AccessToken     string
	CallbackUrl     string

	ConsumerKey     string
	ConsumerSecret  string
}

// RedirectRequired returns a boolean value indicating if the request should
// be redirected to the Provider's login screen, in order to provide an OAuth
// Verifier Token.
func (self *OAuth1Mixin) RedirectRequired(r *http.Request) bool {
	return r.URL.Query().Get("oauth_verifier") == ""
}

// Redirects the User to the OAuth1.0a provider's Login Screen. A RequestToken
// is requested from the Provider, and included in the URL's oauth_token param.
//
// A Successful Login / Authorization should return both the oauth_token and
// the oauth_verifier to the callback URL.
func (self *OAuth1Mixin) AuthorizeRedirect(w http.ResponseWriter, r *http.Request,
	endpoint string, params url.Values) error {

	//create the http request to fetch a Request Token.
	requestTokenUrl, _ := url.Parse(self.RequestToken)
	req := http.Request{
		URL:        requestTokenUrl,
		Method:     "GET",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Close:      true,
	}

	//set the header variables (using defualts), and add the callback URL
	headers := self.headers()
	headers["oauth_callback"] = self.CallbackUrl
	
	//sign the request ...
	key := url.QueryEscape(self.ConsumerSecret) + "&" + url.QueryEscape("")
	base := requestString(req.Method, req.URL.String(), headers)
	headers["oauth_signature"] = sign(base, key)

	//add the Authorization header to the request
	req.Header = http.Header{}
	req.Header.Add("Authorization", authorizationString(headers))

	//make the http request and get the response
	resp, err := http.DefaultClient.Do(&req)
	if err != nil {
		return err
	}

	//get the request body
	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}

	//parse the request token from the body
	parts, err := url.ParseQuery(string(body))
	if err != nil {
		return err
	}

	//now we have the request token, we can re-direct the user to the
	//login screen to authorize us.
	requestToken := parts.Get("oauth_token")
	secretToken := parts.Get("oauth_token_secret")
	if len(requestToken)==0 || len(secretToken)==0 {
		return errors.New(string(body))
	}

	//add the oauth_token_secret to the cache
	tokenCache.Set(requestToken, tokenCacheItem(secretToken))

	//create the URL params, if a nil value was passed to this function	
	if params == nil {
		params = make(url.Values)
	}

	// add the token to the login URL's query parameters
	params.Add("oauth_token", requestToken)

	// create login url
	loginUrl, _ := url.Parse(endpoint)
	loginUrl.RawQuery = params.Encode()

	// redirect to login
	http.Redirect(w, r, loginUrl.String(), http.StatusSeeOther)
	return nil
}

// AuthorizeToken trades the Verification Code (oauth_verification) for an
// Access Token.
func (self *OAuth1Mixin) AuthorizeToken(r *http.Request) (string, string, error) {

	//create the http request to fetch a Request Token.
	accessTokenUrl, _ := url.Parse(self.AccessToken)
	req := http.Request{
		URL:        accessTokenUrl,
		Method:     "GET",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Close:      true,
	}

	//parse oauth data from Redirect URL
	queryParams := r.URL.Query()
	token := queryParams.Get("oauth_token")
	verifier := queryParams.Get("oauth_verifier")

	//get the secret token from the session cache
	cachedSecretToken, ok := tokenCache.Get(token)
	if !ok {
		//TODO throw some kind of exception
	}

	//set the header variables (using defualts), and add the callback URL
	headers := self.headers()
	headers["oauth_token"] = token
	headers["oauth_verifier"] = verifier

	//sign the request ...
	key := url.QueryEscape(self.ConsumerSecret) + "&" + url.QueryEscape(cachedSecretToken.(tokenCacheItem).String())
	base := requestString(req.Method, req.URL.String(), headers)
	headers["oauth_signature"] = sign(base, key)

	//add the Authorization header to the request
	req.Header = http.Header{}
	req.Header.Add("Authorization", authorizationString(headers))
	//req.Header.Add("Content-Type","application/x-www-form-urlencoded")
	//req.Header.Add("Content-Length",strconv.Itoa(len(verifierString)))
	//req.Body = ioutil.NopCloser(strings.NewReader(verifierString))

	//make the http request and get the response
	resp, err := http.DefaultClient.Do(&req)
	if err != nil {
		return "","", err
	}

	//get the request body
	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return "","", err
	}

	//parse the request token from the body
	parts, err := url.ParseQuery(string(body))
	if err != nil {
		return "", "", err
	}

	requestToken := parts.Get("oauth_token")
	secretToken := parts.Get("oauth_token_secret")
	return requestToken, secretToken, nil
}

func (self *OAuth1Mixin) GetAuthenticatedUser(endpoint, token, secret string, resp interface{}) error {

	//create the user url
	endpointUrl, _ := url.Parse(endpoint)

	//create the http request for the user Url
	req := http.Request{
		URL:        endpointUrl,
		Method:     "GET",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Close:      true,
	}

	//set the header variables (using defualts), and add the callback URL
	headers := self.headers()
	headers["oauth_token"] = token

	//sign the request ...
	key := url.QueryEscape(self.ConsumerSecret) + "&" + url.QueryEscape(secret)
	base := requestString(req.Method, req.URL.String(), headers)
	headers["oauth_signature"] = sign(base, key)

	//add the Authorization header to the request
	req.Header = http.Header{}
	req.Header.Add("Authorization", authorizationString(headers))

	//do the http request and get the response
	r, err := http.DefaultClient.Do(&req)
	if err != nil {
		return err
	}

	//get the response body
	userData, err := ioutil.ReadAll(r.Body)
	defer r.Body.Close()
	if err != nil {
		return err
	}

	//unmarshal user json
	return json.Unmarshal(userData, &resp)
}




// Helper Functions ------------------------------------------------------------

func (self *OAuth1Mixin) headers() map[string]string {
	return map[string]string{
		"oauth_consumer_key"     : self.ConsumerKey,
		"oauth_nonce"            : strconv.FormatInt(rand.New(rand.NewSource(time.Now().Unix())).Int63(), 10),
		"oauth_signature_method" : "HMAC-SHA1",
		"oauth_timestamp"        : strconv.FormatInt(time.Now().Unix(), 10),
		"oauth_version"          : "1.0",
	}
}

// Generates an HMAC Signature for an OAuth1.0a request.
func /*(self *OAuth1Mixin)*/ sign(message, key string) string {
	hashfun := hmac.New(sha1.New, []byte(key))
	hashfun.Write([]byte(message))
	rawsignature := hashfun.Sum(nil)
	base64signature := make([]byte, base64.StdEncoding.EncodedLen(len(rawsignature)))
	base64.StdEncoding.Encode(base64signature, rawsignature)
	return string(base64signature)
}





func /*(self *OAuth1Mixin)*/ requestString(method string, uri string, params map[string]string) string {
	
	// loop through params, add keys to map
	var keys []string
	for key, _ := range params {
		keys = append(keys, key)
	}

	// sort the array of header keys
	sort.StringSlice(keys).Sort()

	// create the signed string
	result := method + "&" + url.QueryEscape(uri)

	// loop through sorted params and append to the string
	for pos, key := range keys {
		if pos == 0 {
			result += "&"
		} else {
			result += url.QueryEscape("&")
		}
		result += url.QueryEscape(fmt.Sprintf("%s=%s", key, url.QueryEscape(params[key])))
	}

	return result
}

func /*(self *OAuth1Mixin)*/ authorizationString(params map[string]string) string {
	
	// loop through params, add keys to map
	var keys []string
	for key, _ := range params {
		keys = append(keys, key)
	}

	// sort the array of header keys
	sort.StringSlice(keys).Sort()

	// create the signed string
	result := "OAuth "

	// loop through sorted params and append to the string
	for pos, key := range keys {
		if pos > 0 {
			result += ","
		}
		//result += key + "=\"" + url.QueryEscape(params[key]) + "\""
		result += key + "=\"" + params[key] + "\""
	}

	return result
}

/*
// TODO REMOVE
func url.QueryEscape(s string) string {
	t := make([]byte, 0, 3*len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isEscapable(c) {
			t = append(t, '%')
			t = append(t, "0123456789ABCDEF"[c>>4])
			t = append(t, "0123456789ABCDEF"[c&15])
		} else {
			t = append(t, s[i])
		}
	}
	return string(t)
}

// TODO REMOVE
func isEscapable(b byte) bool {
	return !('A' <= b && b <= 'Z' || 'a' <= b && b <= 'z' || '0' <= b && b <= '9' || b == '-' || b == '.' || b == '_' || b == '~')

}
*/
/*
	// Convert the parameters to a string array
	var authHeaderArray []string
	for key, val := range authHeaders {
		authHeaderStr := fmt.Sprintf(`%s="%s"`, key, val) 
		authHeaderArray = append(authHeaderArray, authHeaderStr)
	}

	authHeader := strings.Join(authHeaderArray, ",")

*/


