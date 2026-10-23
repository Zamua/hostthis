package ssh_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePod is one Solid server and identity provider for alice: a WebID, an
// authorization-code login that approves at once, and storage that checks
// hostthis's tokens the way a pod does (WebID issuer, signature, DPoP).
type fakePod struct {
	t   *testing.T
	srv *httptest.Server
	acp bool // advertise ACP access resources instead of WAC
	// register offers dynamic registration and, as the Nextcloud Solid app
	// does, drops the authorization result for a Client ID Document client.
	register bool
	origin   string

	mu       sync.Mutex
	objects  map[string][]byte // path -> bytes; containers end in "/"
	rules    map[string]string // access resource path -> body
	codes    map[string]string // code -> PKCE challenge
	clients  map[string]string // registered client id -> secret
	userTok  string
	deny     bool // refuse every hostthis request with 403
	agentHit int  // requests authorized as hostthis
}

func newFakePod(t *testing.T, acp, register bool) *fakePod {
	p := &fakePod{t: t, acp: acp, register: register, objects: map[string][]byte{"/alice/": nil}, rules: map[string]string{}, codes: map[string]string{}, clients: map[string]string{}}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	p.origin = strings.Replace(p.srv.URL, "127.0.0.1", "localhost", 1)
	return p
}

func (p *fakePod) webid() string   { return p.origin + "/alice/profile/card#me" }
func (p *fakePod) storage() string { return p.origin + "/alice/" }

func (p *fakePod) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/alice/profile/card":
		w.Header().Set("Content-Type", "text/turtle")
		_, _ = fmt.Fprintf(w, "@prefix solid: <http://www.w3.org/ns/solid/terms#>.\n@prefix pim: <http://www.w3.org/ns/pim/space#>.\n"+
			"<#me> solid:oidcIssuer <%s/idp>; pim:storage </alice/>.\n", p.origin)
		return
	case "/idp/.well-known/openid-configuration":
		cfg := map[string]string{
			"issuer":                 p.origin + "/idp",
			"authorization_endpoint": p.origin + "/idp/auth",
			"token_endpoint":         p.origin + "/idp/token",
		}
		if p.register {
			cfg["registration_endpoint"] = p.origin + "/idp/register"
		}
		writeJSONTo(w, cfg)
		return
	case "/idp/register":
		var req struct {
			RedirectURIs []string `json:"redirect_uris"`
		}
		if !p.register || json.NewDecoder(r.Body).Decode(&req) != nil || len(req.RedirectURIs) != 1 {
			http.Error(w, "bad registration", http.StatusBadRequest)
			return
		}
		id, secret := randHex(8), randHex(16)
		p.mu.Lock()
		p.clients[id] = secret
		p.mu.Unlock()
		writeJSONTo(w, map[string]string{"client_id": id, "client_secret": secret})
		return
	case "/idp/auth":
		p.authorize(w, r)
		return
	case "/idp/token":
		p.token(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/alice/") {
		http.NotFound(w, r)
		return
	}
	p.resource(w, r)
}

func (p *fakePod) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("client_id") == "" {
		http.Error(w, "bad auth request", http.StatusBadRequest)
		return
	}
	if strings.Contains(q.Get("scope"), "offline_access") {
		http.Error(w, "hostthis must not ask for offline_access", http.StatusBadRequest)
		return
	}
	if p.register {
		p.mu.Lock()
		_, registered := p.clients[q.Get("client_id")]
		p.mu.Unlock()
		if !registered {
			// What the Nextcloud Solid app does after its approval page.
			http.Redirect(w, r, q.Get("redirect_uri"), http.StatusFound)
			return
		}
		code := randHex(8)
		p.mu.Lock()
		p.codes[code] = q.Get("code_challenge")
		p.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+q.Get("state"), http.StatusFound)
		return
	}
	// The client must be a Client ID Document naming this redirect.
	res, err := http.Get(q.Get("client_id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var doc struct {
		ClientID     string   `json:"client_id"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	_ = json.NewDecoder(res.Body).Decode(&doc)
	_ = res.Body.Close()
	if doc.ClientID != q.Get("client_id") || len(doc.RedirectURIs) != 1 || doc.RedirectURIs[0] != q.Get("redirect_uri") {
		http.Error(w, "client id document does not match", http.StatusBadRequest)
		return
	}
	code := randHex(8)
	p.mu.Lock()
	p.codes[code] = q.Get("code_challenge")
	p.mu.Unlock()
	http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+q.Get("state"), http.StatusFound)
}

func (p *fakePod) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	p.mu.Lock()
	challenge, ok := p.codes[r.PostForm.Get("code")]
	delete(p.codes, r.PostForm.Get("code"))
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
		w.WriteHeader(http.StatusBadRequest)
		writeJSONTo(w, map[string]string{"error": "invalid_grant"})
		return
	}
	if p.register {
		id, secret, ok := r.BasicAuth()
		p.mu.Lock()
		want, known := p.clients[id]
		p.mu.Unlock()
		if !ok || !known || secret != want {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSONTo(w, map[string]string{"error": "invalid_client"})
			return
		}
	}
	if _, err := verifyProof(r.Header.Get("DPoP"), "POST", p.origin+"/idp/token", ""); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSONTo(w, map[string]string{"error": "invalid_dpop_proof: " + err.Error()})
		return
	}
	claims, _ := json.Marshal(map[string]any{"webid": p.webid(), "iss": p.origin + "/idp"})
	tok := "e30." + base64.RawURLEncoding.EncodeToString(claims) + "." + randHex(8)
	p.mu.Lock()
	p.userTok = tok
	p.mu.Unlock()
	writeJSONTo(w, map[string]string{"access_token": tok, "token_type": "DPoP"})
}

type agent int

const (
	anonymous agent = iota
	owner
	hostthis
)

func (p *fakePod) who(r *http.Request) (agent, error) {
	auth := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(auth, "DPoP ")
	if !ok {
		return anonymous, nil
	}
	if _, err := verifyProof(r.Header.Get("DPoP"), r.Method, p.origin+r.URL.Path, tok); err != nil {
		return anonymous, err
	}
	p.mu.Lock()
	user := tok == p.userTok
	p.mu.Unlock()
	if user {
		return owner, nil
	}
	if err := verifyAgentToken(tok, r.Header.Get("DPoP")); err != nil {
		return anonymous, err
	}
	return hostthis, nil
}

func (p *fakePod) aclOf(path string) string {
	if p.acp {
		return path + ".acr"
	}
	return path + ".acl"
}

// shared reports whether alice's hostthis/ rule grants path to hostthis.
func (p *fakePod) shared(path string) bool {
	rule, ok := p.rules[p.aclOf("/alice/hostthis/")]
	return ok && strings.HasPrefix(path, "/alice/hostthis/") && strings.Contains(rule, "/_solid/id#me") && !strings.HasSuffix(path, ".acl") && !strings.HasSuffix(path, ".acr")
}

func (p *fakePod) resource(w http.ResponseWriter, r *http.Request) {
	who, err := p.who(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	path := r.URL.Path
	p.mu.Lock()
	defer p.mu.Unlock()
	switch who {
	case anonymous:
		w.WriteHeader(http.StatusUnauthorized)
		return
	case hostthis:
		if p.deny || !p.shared(path) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		p.agentHit++
	}
	if strings.HasSuffix(path, ".acl") || strings.HasSuffix(path, ".acr") {
		switch r.Method {
		case http.MethodHead:
			if p.acp {
				w.Header().Set("Link", `<http://www.w3.org/ns/solid/acp#AccessControlResource>; rel="type"`)
			}
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			p.rules[path] = string(body)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="acl"`, p.origin+p.aclOf(path)))
	switch r.Method {
	case http.MethodHead:
		if _, ok := p.objects[path]; !ok {
			w.WriteHeader(http.StatusNotFound)
		}
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		p.objects[path] = body
		// As LDP servers do, a PUT creates the containers above it.
		for dir := path[:strings.LastIndex(path[:len(path)-1], "/")+1]; dir != "/"; dir = dir[:strings.LastIndex(dir[:len(dir)-1], "/")+1] {
			if _, ok := p.objects[dir]; !ok {
				p.objects[dir] = nil
			}
		}
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		body, ok := p.objects[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.HasSuffix(path, "/") {
			var members []map[string]string
			for k := range p.objects {
				if k != path && strings.HasPrefix(k, path) && !strings.Contains(strings.TrimSuffix(k[len(path):], "/"), "/") {
					members = append(members, map[string]string{"@id": p.origin + k})
				}
			}
			w.Header().Set("Content-Type", "application/ld+json")
			writeJSONTo(w, []map[string]any{{"@id": p.origin + path, "http://www.w3.org/ns/ldp#contains": members}})
			return
		}
		if rng := r.Header.Get("Range"); rng != "" {
			m := regexp.MustCompile(`^bytes=(\d+)-(\d+)$`).FindStringSubmatch(rng)
			from, _ := strconv.Atoi(m[1])
			to, _ := strconv.Atoi(m[2])
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, len(body)))
			w.Header().Set("Content-Length", strconv.Itoa(to-from+1))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(body[from : to+1])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	case http.MethodDelete:
		if _, ok := p.objects[path]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(p.objects, path)
		w.WriteHeader(http.StatusResetContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// podKeys are the object paths stored under alice's hostthis/ folder, files
// only.
func (p *fakePod) podKeys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var keys []string
	for k := range p.objects {
		if rest, ok := strings.CutPrefix(k, "/alice/hostthis/"); ok && rest != "" && !strings.HasSuffix(k, "/") {
			keys = append(keys, rest)
		}
	}
	sort.Strings(keys)
	return keys
}

func (p *fakePod) setDeny(deny bool) {
	p.mu.Lock()
	p.deny = deny
	p.mu.Unlock()
}

func writeJSONTo(w http.ResponseWriter, v any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	_ = json.NewEncoder(w).Encode(v)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// --- token checks a pod makes --------------------------------------------------

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (k jwk) key() (*ecdsa.PublicKey, error) {
	if k.Kty != "EC" || k.Crv != "P-256" {
		return nil, errors.New("not a P-256 key")
	}
	x, err1 := base64.RawURLEncoding.DecodeString(k.X)
	y, err2 := base64.RawURLEncoding.DecodeString(k.Y)
	if err1 != nil || err2 != nil {
		return nil, errors.New("bad key coordinates")
	}
	return ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
}

func (k jwk) thumbprint() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf(`{"crv":%q,"kty":%q,"x":%q,"y":%q}`, k.Crv, k.Kty, k.X, k.Y)))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func splitJWT(tok string, key *ecdsa.PublicKey) (header, claims map[string]any, err error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, nil, errors.New("not a JWT")
	}
	h, _ := base64.RawURLEncoding.DecodeString(parts[0])
	c, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if json.Unmarshal(h, &header) != nil || json.Unmarshal(c, &claims) != nil {
		return nil, nil, errors.New("undecodable JWT")
	}
	if key == nil {
		raw, _ := json.Marshal(header["jwk"])
		var k jwk
		_ = json.Unmarshal(raw, &k)
		if key, err = k.key(); err != nil {
			return nil, nil, err
		}
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(sig) != 64 || !ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return nil, nil, errors.New("bad signature")
	}
	return header, claims, nil
}

// verifyProof checks a DPoP proof for one request and returns its key's
// thumbprint.
func verifyProof(proof, method, url, accessToken string) (string, error) {
	header, claims, err := splitJWT(proof, nil)
	if err != nil {
		return "", fmt.Errorf("dpop: %w", err)
	}
	if header["typ"] != "dpop+jwt" || header["alg"] != "ES256" || claims["htm"] != method || claims["htu"] != url {
		return "", fmt.Errorf("dpop: proof for %v %v, request %s %s", claims["htm"], claims["htu"], method, url)
	}
	if accessToken != "" {
		sum := sha256.Sum256([]byte(accessToken))
		if claims["ath"] != base64.RawURLEncoding.EncodeToString(sum[:]) {
			return "", errors.New("dpop: ath does not match the token")
		}
	}
	raw, _ := json.Marshal(header["jwk"])
	var k jwk
	_ = json.Unmarshal(raw, &k)
	return k.thumbprint(), nil
}

var issuerRe = regexp.MustCompile(`solid:oidcIssuer <([^>]+)>`)

// verifyAgentToken checks a token as a resource server would: the issuer is
// the one its WebID names, the signature verifies against that issuer's keys,
// and it is bound to the proof's key.
func verifyAgentToken(tok, proof string) error {
	_, unverified, err := splitJWTUnverified(tok)
	if err != nil {
		return err
	}
	webid, _ := unverified["webid"].(string)
	iss, _ := unverified["iss"].(string)
	profile, err := httpGetBody(strings.Split(webid, "#")[0])
	if err != nil {
		return err
	}
	if m := issuerRe.FindStringSubmatch(profile); m == nil || m[1] != iss {
		return fmt.Errorf("WebID %s does not name issuer %s", webid, iss)
	}
	cfgBody, err := httpGetBody(iss + "/.well-known/openid-configuration")
	if err != nil {
		return err
	}
	var cfg struct {
		JWKSURI string `json:"jwks_uri"`
	}
	_ = json.Unmarshal([]byte(cfgBody), &cfg)
	keysBody, err := httpGetBody(cfg.JWKSURI)
	if err != nil {
		return err
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	_ = json.Unmarshal([]byte(keysBody), &set)
	if len(set.Keys) != 1 {
		return errors.New("want one issuer key")
	}
	key, err := set.Keys[0].key()
	if err != nil {
		return err
	}
	_, claims, err := splitJWT(tok, key)
	if err != nil {
		return fmt.Errorf("access token: %w", err)
	}
	if claims["aud"] != "solid" {
		return errors.New("access token audience is not solid")
	}
	if exp, _ := claims["exp"].(float64); int64(exp) < time.Now().Unix() {
		return errors.New("access token expired")
	}
	thumb, err := verifyProofKey(proof)
	if err != nil {
		return err
	}
	cnf, _ := claims["cnf"].(map[string]any)
	if cnf["jkt"] != thumb {
		return errors.New("access token is not bound to the proof's key")
	}
	return nil
}

func verifyProofKey(proof string) (string, error) {
	header, _, err := splitJWTUnverified(proof)
	if err != nil {
		return "", err
	}
	raw, _ := json.Marshal(header["jwk"])
	var k jwk
	_ = json.Unmarshal(raw, &k)
	return k.thumbprint(), nil
}

func splitJWTUnverified(tok string) (header, claims map[string]any, err error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, nil, errors.New("not a JWT")
	}
	h, _ := base64.RawURLEncoding.DecodeString(parts[0])
	c, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if json.Unmarshal(h, &header) != nil || json.Unmarshal(c, &claims) != nil {
		return nil, nil, errors.New("undecodable JWT")
	}
	return header, claims, nil
}

func httpGetBody(url string) (string, error) {
	res, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer res.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %d", url, res.StatusCode)
	}
	return string(body), err
}
