package main

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ---------------- onvif: raw SOAP client (stdlib only) -------

// ONVIF namespaces.
const (
	onvifNsDev    = "http://www.onvif.org/ver10/device/wsdl"
	onvifNsMedia  = "http://www.onvif.org/ver10/media/wsdl"
	onvifNsPtz    = "http://www.onvif.org/ver20/ptz/wsdl"
	onvifNsEv     = "http://www.onvif.org/ver10/events/wsdl"
	onvifNsSchema = "http://www.onvif.org/ver10/schema"
)

// Precompiled patterns for ONVIF response parsing. These run per probe and
// per event, so they are compiled once at package load instead of via
// regexp.MustCompile on every call. onvifCapXAddr stays dynamic (the section
// name is interpolated) and is cached per section in onvifCapReCache.
var (
	onvifCapXAddrFmt    = `<(?:[^:>]+:)?%s\b[^>]*>\s*<(?:[^:>]+:)?XAddr[^>]*>\s*([^<]+?)\s*</(?:[^:>]+:)?XAddr>`
	onvifProfileTokenRe = regexp.MustCompile(`<(?:[^:>]+:)?Profiles\b[^>]*\btoken="([^"]+)"`)
	onvifSnapshotURIRe  = regexp.MustCompile(`<(?:[^:>]+:)?Uri[^>]*>\s*([^<]+?)\s*</(?:[^:>]+:)?Uri>`)
	onvifWSAddrRe       = regexp.MustCompile(`<(?:[^>]*:)?Address[^>]*>\s*([^<]+?)\s*</(?:[^>]*:)?Address>`)
	onvifDigestKVRe     = regexp.MustCompile(`(\w+)\s*=\s*(?:"([^"]*)"|([^,\s]+))`)
	onvifCapReCache     sync.Map // string -> *regexp.Regexp
)

// onvifCapXAddrRe returns the cached XAddr extractor for one capability
// section (Media, PTZ, Events), compiling it once.
func onvifCapXAddrRe(cap string) *regexp.Regexp {
	if v, ok := onvifCapReCache.Load(cap); ok {
		return v.(*regexp.Regexp)
	}
	var re = regexp.MustCompile(fmt.Sprintf(onvifCapXAddrFmt, cap))
	actual, _ := onvifCapReCache.LoadOrStore(cap, re)
	return actual.(*regexp.Regexp)
}

// onvifDevice holds the discovered service XAddrs and the media profile token
// for a single camera. discovery is lazy and cached.
type onvifDevice struct {
	cam         *CameraCfg
	deviceX     string // base device service endpoint
	mediaX      string // media service XAddr
	ptzX        string // ptz service XAddr
	evtX        string // events service XAddr
	profile     string // first media profile token
	snapshotUri string // GetSnapshotUri result
	err         error
	// discover is true once a SUCCESSFUL discovery has been cached. A failed
	// discovery is retried (throttled by nextRetry) instead of being cached:
	// cameras usually boot slower than the NVR and must not be permanently
	// poisoned by one failed first probe.
	discover  bool
	nextRetry time.Time
	mu        sync.Mutex
	// discMtx serializes the discovery round trips so mu is never held across
	// network I/O (a SOAP probe takes seconds and would block every reader).
	discMtx sync.Mutex
}

// snapshotURI returns the cached GetSnapshotUri result.
func (d *onvifDevice) snapshotURI() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snapshotUri
}

// setSnapshotURI caches a snapshot URI discovered outside discoverSelf.
func (d *onvifDevice) setSnapshotURI(u string) {
	d.mu.Lock()
	d.snapshotUri = u
	d.mu.Unlock()
}

// cachedErr returns the last discovery error (nil after a successful discovery).
func (d *onvifDevice) cachedErr() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

// profileToken returns the cached media profile token ("" when unknown).
func (d *onvifDevice) profileToken() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.profile
}

// mediaEndpoint returns the cached media service XAddr ("" when unknown).
func (d *onvifDevice) mediaEndpoint() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mediaX
}

// ptzEndpoint returns the cached PTZ service XAddr ("" when unknown).
func (d *onvifDevice) ptzEndpoint() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ptzX
}

// eventsEndpoint returns the cached events service XAddr ("" when unknown).
func (d *onvifDevice) eventsEndpoint() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.evtX
}

// onvifDevices caches discovered devices per camera endpoint (ONVIF URL,
// falling back to name). Keying by name collides when two cameras share a
// name and orphans the entry on rename; the URL is stable and unique.
var onvifDevices = struct {
	sync.Mutex
	m map[string]*onvifDevice
}{m: map[string]*onvifDevice{}}

func onvifDeviceKey(cam *CameraCfg) string {
	if cam == nil {
		return ""
	}
	if u := cam.Onvif.Url; u != "" {
		return "url:" + u
	}
	return "name:" + cam.Name
}

// onvifGet lazily returns the (discovering) device handle for a camera.
// Creation is atomic and discovery runs outside any lock shared with readers.
func onvifGet(cam *CameraCfg) *onvifDevice {
	var key = onvifDeviceKey(cam)
	onvifDevices.Lock()
	dev, ok := onvifDevices.m[key]
	if !ok {
		dev = &onvifDevice{cam: cam}
		onvifDevices.m[key] = dev
	}
	onvifDevices.Unlock()

	// Serialize discovery without holding readers across SOAP round trips: if
	// another goroutine is already probing, return the last-known cached
	// values immediately instead of queueing behind seconds of network I/O.
	// Callers treat an undiscovered device as a retryable miss.
	if !dev.discMtx.TryLock() {
		return dev
	}
	defer dev.discMtx.Unlock()

	dev.mu.Lock()
	need := !dev.discover && time.Now().After(dev.nextRetry)
	dev.mu.Unlock()
	if !need {
		return dev
	}
	// Probe into a scratch device, then publish the result under mu.
	var d onvifDevice
	d.cam = cam
	d.discoverSelf()
	dev.mu.Lock()
	dev.deviceX, dev.mediaX, dev.ptzX, dev.evtX = d.deviceX, d.mediaX, d.ptzX, d.evtX
	dev.profile, dev.snapshotUri = d.profile, d.snapshotUri
	dev.err = d.err
	dev.discover = d.err == nil
	if d.err != nil {
		dev.nextRetry = time.Now().Add(5 * time.Second)
	}
	dev.mu.Unlock()
	return dev
}

// discoverSelf queries GetCapabilities to resolve service XAddrs, then loads a
// media profile token with GetProfiles. All endpoints default to the shared
// ONVIF service URI (as Tapo devices expose many ports on /onvif/service).
func (d *onvifDevice) discoverSelf() {
	if d.cam.Onvif.Url == "" {
		d.err = fmt.Errorf("no onvif url for %s", d.cam.Name)
		return
	}
	var u, cerr = url.Parse(d.cam.Onvif.Url)
	if cerr != nil || u.Host == "" {
		d.err = fmt.Errorf("bad onvif url %q", d.cam.Onvif.Url)
		return
	}
	var user, pass = d.cam.Onvif.Username, d.cam.Onvif.Password

	// The base ONVIF service URL is derived from the configured URL. Most Tapo
	// firmwares bind devicemgmt to /device_service and everything else on
	// /service; probe both for the device service to fetch capabilities.
	var deviceCandidates = []string{
		d.cam.Onvif.Url,
		"http://" + u.Host + "/onvif/device_service",
		"http://" + u.Host + "/onvif/service",
	}
	for _, ep := range deviceCandidates {
		raw, err := onvifSoap(ep, onvifNsDev+"/GetCapabilities",
			"<dev:GetCapabilities xmlns:dev=\""+onvifNsDev+"\"><dev:Category>All</dev:Category></dev:GetCapabilities>",
			user, pass)
		if err != nil {
			continue
		}
		d.deviceX = ep
		d.mediaX = onvifCapXAddr(raw, "Media")
		d.ptzX = onvifCapXAddr(raw, "PTZ")
		d.evtX = onvifCapXAddr(raw, "Events")
		break
	}
	// Fallback: assume a single shared ONVIF service endpoint.
	if d.mediaX == "" {
		d.mediaX = "http://" + u.Host + "/onvif/service"
	}
	if d.ptzX == "" {
		d.ptzX = d.mediaX
	}
	if d.evtX == "" {
		d.evtX = "http://" + u.Host + "/onvif/events_service"
	}

	// Resolve a media profile token.
	for _, ep := range []string{d.mediaX, d.deviceX} {
		if ep == "" {
			continue
		}
		raw, err := onvifSoap(ep, onvifNsMedia+"/GetProfiles",
			"<m:GetProfiles xmlns:m=\""+onvifNsMedia+"\"/>", user, pass)
		if err != nil {
			continue
		}
		if tok := onvifFirstProfileToken(raw); tok != "" {
			d.profile = tok
			break
		}
	}
	if d.profile == "" || d.mediaX == "" {
		d.err = fmt.Errorf("onvif discovery failed for %s (no media endpoint/profile)", d.cam.Name)
		return
	}
	// Best-effort: resolve snapshot URI (used for native JPEG without ffmpeg/go2rtc)
	for _, ep := range []string{d.mediaX, d.deviceX} {
		if ep == "" {
			continue
		}
		raw, err := onvifSoap(ep, onvifNsMedia+"/GetSnapshotUri",
			"<m:GetSnapshotUri xmlns:m=\""+onvifNsMedia+"\"><m:ProfileToken>"+xmlEscape(d.profile)+"</m:ProfileToken></m:GetSnapshotUri>", user, pass)
		if err != nil {
			continue
		}
		if uri := onvifSnapshotURI(raw); uri != "" {
			// Fix Tapo quirk returning 0.0.0.0 or empty host: swap in the
			// camera hostname only (u.Host carries the ONVIF port, which
			// must not be merged with the snapshot URI's own port).
			if strings.Contains(uri, "0.0.0.0") {
				uri = strings.Replace(uri, "0.0.0.0", u.Hostname(), 1)
			} else if su, perr := url.Parse(uri); perr == nil && su.Hostname() == "" {
				su.Host = u.Host
				uri = su.String()
			}
			d.snapshotUri = strings.TrimSpace(uri)
			break
		}
	}
}

// onvifProfileToken returns the first media profile token for the camera.
func onvifProfileToken(cam *CameraCfg) (string, error) {
	var dev = onvifGet(cam)
	if e := dev.cachedErr(); e != nil {
		return "", e
	}
	var tok = dev.profileToken()
	if tok == "" {
		return "", fmt.Errorf("no media profile for %s", cam.Name)
	}
	return tok, nil
}

// onvifPTZMove sends a ContinuousMove velocity command. x (pan), y (tilt) are
// in the ONVIF [-1,1] range.
func onvifPTZMove(cam *CameraCfg, x, y float32) error {
	var dev = onvifGet(cam)
	if e := dev.cachedErr(); e != nil {
		return e
	}
	tok, err := onvifProfileToken(cam)
	if err != nil {
		return err
	}
	var body = "<ptz:ContinuousMove xmlns:ptz=\"" + onvifNsPtz + "\" xmlns:tt=\"" + onvifNsSchema + "\">" +
		"<ptz:ProfileToken>" + xmlEscape(tok) + "</ptz:ProfileToken>" +
		"<ptz:Velocity><tt:PanTilt x=\"" + fmt.Sprintf("%g", x) + "\" y=\"" + fmt.Sprintf("%g", y) + "\"/><tt:Zoom x=\"0\"/></ptz:Velocity>" +
		"</ptz:ContinuousMove>"
	_, err = onvifSoap(dev.ptzEndpoint(), onvifNsPtz+"/ContinuousMove", body, cam.Onvif.Username, cam.Onvif.Password)
	return err
}

// onvifPTZHome moves the camera to its configured home position.
func onvifPTZHome(cam *CameraCfg) error {
	var dev = onvifGet(cam)
	if e := dev.cachedErr(); e != nil {
		return e
	}
	tok, err := onvifProfileToken(cam)
	if err != nil {
		return err
	}
	var body = "<ptz:GotoHomePosition xmlns:ptz=\"" + onvifNsPtz + "\">" +
		"<ptz:ProfileToken>" + xmlEscape(tok) + "</ptz:ProfileToken>" +
		"</ptz:GotoHomePosition>"
	_, err = onvifSoap(dev.ptzEndpoint(), onvifNsPtz+"/GotoHomePosition", body, cam.Onvif.Username, cam.Onvif.Password)
	return err
}

// onvifPTZStop halts all PTZ movement.
func onvifPTZStop(cam *CameraCfg) error {
	var dev = onvifGet(cam)
	if e := dev.cachedErr(); e != nil {
		return e
	}
	tok, err := onvifProfileToken(cam)
	if err != nil {
		return err
	}
	var body = "<ptz:Stop xmlns:ptz=\"" + onvifNsPtz + "\">" +
		"<ptz:ProfileToken>" + xmlEscape(tok) + "</ptz:ProfileToken>" +
		"<ptz:PanTilt>true</ptz:PanTilt><ptz:Zoom>true</ptz:Zoom>" +
		"</ptz:Stop>"
	_, err = onvifSoap(dev.ptzEndpoint(), onvifNsPtz+"/Stop", body, cam.Onvif.Username, cam.Onvif.Password)
	return err
}

// onvifCapXAddr extracts the <XAddr> URI for a named capability section
// (Media, PTZ, Events) from a GetCapabilities response.
func onvifCapXAddr(raw []byte, cap string) string {
	if m := onvifCapXAddrRe(cap).FindSubmatch(raw); m != nil {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

// onvifFirstProfileToken extracts the first <Profiles token="...">.
func onvifFirstProfileToken(raw []byte) string {
	if m := onvifProfileTokenRe.FindSubmatch(raw); m != nil {
		return string(m[1])
	}
	return ""
}

// onvifSnapshotURI extracts the <Uri> from a GetSnapshotUri response.
func onvifSnapshotURI(raw []byte) string {
	if m := onvifSnapshotURIRe.FindSubmatch(raw); m != nil {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

// onvifWSAddr extracts the WS-Addressing <Address> URI from a response body.
func onvifWSAddr(raw []byte) string {
	if m := onvifWSAddrRe.FindSubmatch(raw); m != nil {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

// digest helpers for native snapshot GET (HTTP Digest auth, stdlib only)
func digestParts(header string) map[string]string {
	// header is like: Digest realm="...", nonce="...", qop="auth,auth-int", algorithm=MD5
	m := map[string]string{}
	// strip "Digest "
	if idx := strings.Index(strings.ToLower(header), "digest "); idx != -1 {
		header = header[idx+7:]
	}
	// Quoted values are matched up to the closing quote so a value containing
	// commas (qop="auth,auth-int") is not truncated at the first comma.
	for _, pm := range onvifDigestKVRe.FindAllStringSubmatch(header, -1) {
		v := pm[2]
		if v == "" {
			v = pm[3]
		}
		m[strings.ToLower(pm[1])] = v
	}
	return m
}

func md5Hex(s string) string {
	h := md5.New()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// httpGetDigest performs a GET with HTTP Digest auth (MD5/qop=auth).
func httpGetDigest(uri, user, pass string, timeout time.Duration) ([]byte, string, error) {
	client := &http.Client{Timeout: timeout}
	// first request to get 401 + nonce
	req, rerr := http.NewRequest("GET", uri, nil)
	if rerr != nil {
		return nil, "", rerr
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	// if not 401, return body directly if 200
	if resp.StatusCode != 401 {
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, "", fmt.Errorf("http status %d", resp.StatusCode)
		}
		data, derr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if derr != nil {
			return nil, "", derr
		}
		return data, resp.Header.Get("Content-Type"), nil
	}
	wwwAuth := resp.Header.Get("Www-Authenticate")
	if wwwAuth == "" {
		wwwAuth = resp.Header.Get("WWW-Authenticate")
	}
	resp.Body.Close()
	if wwwAuth == "" {
		return nil, "", fmt.Errorf("no digest challenge")
	}
	parts := digestParts(wwwAuth)
	realm := parts["realm"]
	nonce := parts["nonce"]
	qop := parts["qop"]
	algo := parts["algorithm"]
	opaque := parts["opaque"]
	if realm == "" || nonce == "" {
		return nil, "", fmt.Errorf("bad digest challenge")
	}
	// RFC 7616: MD5 and SHA-256 (each with the optional -sess variant) are what
	// cameras actually use. Anything else fails loudly instead of silently
	// computing a response the camera will refuse.
	var hash = md5Hex
	var algoLower = strings.ToLower(algo)
	switch strings.TrimSuffix(algoLower, "-sess") {
	case "", "md5":
		hash = md5Hex
	case "sha-256", "sha256":
		hash = sha256Hex
	default:
		return nil, "", fmt.Errorf("unsupported digest algorithm %q", algo)
	}
	var sess = strings.HasSuffix(algoLower, "-sess")
	// parse uri path
	uu, uerr := url.Parse(uri)
	if uerr != nil || uu == nil {
		return nil, "", fmt.Errorf("bad uri %q", uri)
	}
	uriPath := uu.RequestURI()
	if uriPath == "" {
		uriPath = "/"
	}
	// cnonce
	var cnonceBytes = make([]byte, 8)
	if _, rerr := rand.Read(cnonceBytes); rerr != nil {
		return nil, "", rerr
	}
	cnonce := hex.EncodeToString(cnonceBytes)
	nc := "00000001"
	ha1 := hash(user + ":" + realm + ":" + pass)
	if sess {
		// RFC 7616 3.4.2: the session variant folds nonce+cnonce into HA1.
		ha1 = hash(ha1 + ":" + nonce + ":" + cnonce)
	}
	ha2 := hash("GET:" + uriPath)
	var response string
	// qop may list several options ("auth,auth-int"); prefer auth.
	if qop != "" && strings.Contains(qop, "auth") {
		response = hash(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":auth:" + ha2)
	} else {
		response = hash(ha1 + ":" + nonce + ":" + ha2)
	}
	authHeader := fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`, user, realm, nonce, uriPath, response)
	if qop != "" {
		authHeader += fmt.Sprintf(`, qop=auth, nc=%s, cnonce="%s"`, nc, cnonce)
	}
	if opaque != "" {
		authHeader += fmt.Sprintf(`, opaque="%s"`, opaque)
	}
	if algo != "" {
		authHeader += fmt.Sprintf(`, algorithm=%s`, algo)
	}
	req2, rerr2 := http.NewRequest("GET", uri, nil)
	if rerr2 != nil {
		return nil, "", rerr2
	}
	req2.Header.Set("Authorization", authHeader)
	resp2, err := client.Do(req2)
	if err != nil {
		return nil, "", err
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		return nil, "", fmt.Errorf("digest http status %d", resp2.StatusCode)
	}
	data, derr2 := io.ReadAll(io.LimitReader(resp2.Body, 8<<20))
	if derr2 != nil {
		return nil, "", derr2
	}
	return data, resp2.Header.Get("Content-Type"), nil
}

// xmlEscape escapes a value for inclusion in an XML text node or attribute.
// ONVIF usernames, passwords-in-URLs and profile tokens come from user config;
// without escaping a value containing "&" or "<" breaks every SOAP request.
func xmlEscape(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return s
	}
	return b.String()
}

// onvifSoap is the stdlib raw-SOAP transport: it posts a WS-Security-signed
// envelope to addr and returns the response bytes, treating HTTP/Envelope
// faults as errors.
func onvifSoap(addr, action, body, user, pass string) ([]byte, error) {
	var nonceBytes = make([]byte, 16)
	_, _ = rand.Read(nonceBytes)
	var nonce = base64.StdEncoding.EncodeToString(nonceBytes)
	var created = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	var h = sha1.New()
	h.Write(nonceBytes)
	h.Write([]byte(created))
	h.Write([]byte(pass))
	var digest = base64.StdEncoding.EncodeToString(h.Sum(nil))

	var soap = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>"
	soap += "<s:Envelope xmlns:s=\"http://www.w3.org/2003/05/soap-envelope\" xmlns:wsa=\"http://www.w3.org/2005/08/addressing\">"
	soap += "<s:Header>"
	soap += "<wsa:To>" + xmlEscape(addr) + "</wsa:To>"
	soap += "<wsa:Action>" + xmlEscape(action) + "</wsa:Action>"
	soap += "<o:Security xmlns:o=\"http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd\">"
	soap += "<o:UsernameToken xmlns:o=\"http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd\"><o:Username>" + xmlEscape(user) + "</o:Username>"
	soap += "<o:Password Type=\"http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest\">" + digest + "</o:Password>"
	soap += "<o:Nonce EncodingType=\"http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary\">" + nonce + "</o:Nonce>"
	soap += "<u:Created xmlns:u=\"http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd\">" + created + "</u:Created>"
	soap += "</o:UsernameToken></o:Security></s:Header>"
	soap += "<s:Body>" + body + "</s:Body></s:Envelope>"

	var req, err = http.NewRequest("POST", addr, strings.NewReader(soap))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")
	req.Header.Set("SOAPAction", "\""+action+"\"")
	req.Header.Set("Connection", "close")
	var client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	var resp, rerr = client.Do(req)
	if rerr != nil {
		return nil, rerr
	}
	defer resp.Body.Close()
	var data, derr = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if derr != nil {
		return nil, derr
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("soap status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if strings.Contains(strings.ToLower(string(data)), "<s:fault>") || strings.Contains(strings.ToLower(string(data)), "<soap-env:fault>") {
		return nil, fmt.Errorf("soap fault: %s", strings.TrimSpace(string(data)))
	}
	return data, nil
}

// onvifEventSubscribe creates a PullPoint subscription as a fallback when the
// webhook is unsupported. The Events endpoint comes from GetCapabilities; some
// Tapo firmwares only accept the subscription when InitialTerminationTime is
// either present or absent, so we try both. Returns the subscription (pull-point)
// URI.
func onvifEventSubscribe(cam *CameraCfg) (string, error) {
	dev := onvifGet(cam)
	if e := dev.cachedErr(); e != nil {
		return "", e
	}
	var ev = dev.eventsEndpoint()
	if ev == "" {
		ev = cam.Onvif.Url + "/onvif/events_service"
	}
	var user, pass = cam.Onvif.Username, cam.Onvif.Password
	var term = time.Now().UTC().Add(time.Hour).Format("2006-01-02T15:04:05Z")
	var bodies = []string{
		"<tev:CreatePullPointSubscription xmlns:tev=\"" + onvifNsEv + "\"/>",
		"<tev:CreatePullPointSubscription xmlns:tev=\"" + onvifNsEv + "\" xmlns:wsnt=\"http://docs.oasis-open.org/wsn/b-2\">" +
			"<wsnt:InitialTerminationTime>" + term + "</wsnt:InitialTerminationTime></tev:CreatePullPointSubscription>",
	}
	var lastErr error
	for _, body := range bodies {
		raw, err := onvifSoap(ev, onvifNsEv+"/CreatePullPointSubscription", body, user, pass)
		if err != nil {
			lastErr = err
			continue
		}
		if u := onvifWSAddr(raw); u != "" {
			return u, nil
		}
		lastErr = fmt.Errorf("no subscription address: %s", strings.TrimSpace(string(raw)))
	}
	return "", fmt.Errorf("onvif events endpoint=%s: %v", ev, lastErr)
}

// onvifPullOnce polls the subscription URI for one batch of messages and
// returns the raw notification XML ("" when there is nothing of interest).
func onvifPullOnce(uri string, cam *CameraCfg) ([]byte, error) {
	var user, pass = cam.Onvif.Username, cam.Onvif.Password
	var body = "<tev:PullMessages xmlns:tev=\"http://www.onvif.org/ver10/events/wsdl\">" +
		"<tev:Timeout>PT3S</tev:Timeout><tev:MessageLimit>32</tev:MessageLimit></tev:PullMessages>"
	raw, err := onvifSoap(uri, "http://www.onvif.org/ver10/events/wsdl/PullMessages", body, user, pass)
	if err != nil {
		// A camera may return a fault when the pull window simply had no events.
		if strings.Contains(strings.ToLower(err.Error()), "fault") {
			return nil, nil
		}
		return nil, err
	}
	return raw, nil
}

// onvifTopicType maps an ONVIF event topic (last path element or key segment)
// to a detection type: motion / person / vehicle / pet, or "other" for a
// recognized detection topic we have no better label for.
//
// It returns "" for anything that is NOT a detection (tamper alarms, SD-card
// events, relay/trigger topics, ...): those must not create recordings.
func onvifTopicType(topic string) string {
	var t = strings.ToLower(topic)
	if t == "" {
		return ""
	}
	var keys = []struct{ key, kind string }{
		{"person", "person"}, {"people", "person"}, {"human", "person"},
		{"vehicle", "vehicle"}, {"automobile", "vehicle"}, {"truck", "vehicle"},
		{"bus", "vehicle"}, {"bicycle", "vehicle"}, {"motorbike", "vehicle"},
		{"pet", "pet"}, {"dog", "pet"}, {"cat", "pet"}, {"animal", "pet"},
		{"motion", "motion"}, {"movement", "motion"},
		{"linecross", "motion"}, {"line_cross", "motion"}, {"crossline", "motion"},
		{"linedetector", "motion"}, {"crossing", "motion"},
		{"intrusion", "motion"}, {"regionenter", "motion"}, {"regionexit", "motion"},
		{"regiondetector", "motion"}, {"fielddetector", "motion"}, {"objectsinside", "motion"},
		{"loitering", "motion"}, {"missing", "motion"},
		{"face", "other"}, {"cry", "other"}, {"glass", "other"}, {"shatter", "other"},
		{"sound", "other"}, {"audio", "other"}, {"smoke", "other"},
	}
	for _, k := range keys {
		if strings.Contains(t, k.key) {
			return k.kind
		}
	}
	// Anything else (Device/Trigger/Relay, Storage/..., Tampering, ...) is not
	// a detection and is ignored rather than reported as "other".
	return ""
}

// onvifEventActive parses an ONVIF Notify body and reports the detection type
// when the notification is a positive (State=true) motion/person/vehicle/pet
// event. Tapo cameras send a Payload with a Topic (e.g.
// tns1:RuleEngine/CellMotionDetector/Motion) and a SimpleItem State=true.
func onvifEventActive(raw []byte) (string, bool) {
	if raw == nil || len(raw) == 0 {
		return "", false
	}
	var topic string
	var data []xmlSimpleItem
	inTopic := false
	var topicSb strings.Builder
	dec := xml.NewDecoder(bytes.NewReader(raw))
	for {
		tok, err := dec.Token()
		if err != nil {
			break // EOF or parse error: fall through using collected data
		}
		switch el := tok.(type) {
		case xml.StartElement:
			switch el.Name.Local {
			case "Topic":
				inTopic = true
				topicSb.Reset()
			case "SimpleItem":
				var it xmlSimpleItem
				for _, a := range el.Attr {
					switch a.Name.Local {
					case "Name":
						it.Name = a.Value
					case "Value":
						it.Value = a.Value
					}
				}
				data = append(data, it)
			}
		case xml.CharData:
			if inTopic {
				topicSb.Write([]byte(el))
			}
		case xml.EndElement:
			if inTopic && el.Name.Local == "Topic" {
				inTopic = false
				topic = strings.TrimSpace(topicSb.String())
			}
		}
	}
	var dtype = onvifTopicType(topic)
	if dtype == "" {
		// No recognized topic; fall back to inspecting State-bearing items.
		for _, it := range data {
			if d := onvifTopicType(it.Name + " " + it.Value); d != "" {
				dtype = d
				break
			}
		}
	}
	// A detection requires a "true" state item.
	for _, it := range data {
		// ONVIF boolean state items are named variously: "State"/"Active"
		// (generic), but the CellMotionDetector uses "IsMotion" and some
		// firmwares use "Alarm". Without these, an IsMotion=false stop
		// notification fell through to the "topic implies active" path below
		// and started a spurious detection that never cleared.
		if lname := strings.ToLower(it.Name); strings.Contains(lname, "state") || strings.Contains(lname, "active") || strings.Contains(lname, "motion") || strings.Contains(lname, "alarm") {
			if strings.EqualFold(it.Value, "true") {
				return dtype, true
			}
			// Explicit false ends the event: not a fresh detection.
			if strings.EqualFold(it.Value, "false") {
				return dtype, false
			}
		}
	}
	// Some Tapo firmwares only emit the true notification without a State item.
	if topic != "" && dtype != "" {
		return dtype, true
	}
	return dtype, false
}

// xmlSimpleItem is a parsed <tt:SimpleItem Name=".." Value=".."/>.
type xmlSimpleItem struct {
	Name  string
	Value string
}

// NOTE: an earlier 5s per-camera eventDebounce lived here but was never wired
// in: dropping raw events would break grouping extensions (clip end times),
// and the post-detection grouping window already subsumes its intent.

// camActive tracks whether a camera currently reports an active detection.
// While true, assembleClip defers the post-detection countdown. Keyed by
// source (onvif / rtsp) so that with detection.source=both one source clearing
// cannot end a detection the other still reports as active.
var camActiveMtx sync.Mutex
var camActive = map[string]map[string]bool{}

// setCamActive is the ONVIF-source shorthand.
func setCamActive(camID string, active bool) { setCamActiveSource(camID, "onvif", active) }

// setCamActiveSource sets the active flag of one detection source.
func setCamActiveSource(camID string, source string, active bool) {
	camActiveMtx.Lock()
	var m = camActive[camID]
	if m == nil {
		m = map[string]bool{}
		camActive[camID] = m
	}
	m[source] = active
	camActiveMtx.Unlock()
}

// camIsActive reports whether any source currently reports an active detection.
func camIsActive(camID string) bool {
	camActiveMtx.Lock()
	defer camActiveMtx.Unlock()
	for _, v := range camActive[camID] {
		if v {
			return true
		}
	}
	return false
}

// onvifEventLoop drives motion detection for one camera. It prefers the
// push-based WS-Base webhook (camera POSTs events to our /onvif/notify URL):
// this is the only working path on Tapo firmwares where PullPoint returns a
// fault (ter:InvalidArgVal). PullPoint is used only as a fallback. The two are
// never run at the same time; some Tapo firmwares freeze when both a PullPoint
// connection and a webhook are created in sequence.
func onvifEventLoop(app *App, cam *CameraCfg, camID string) {
	var lastLog int64
	for {
		// 1) Try the webhook (preferred). If it succeeds, events arrive via the
		// HTTP notify endpoint and we only need to renew the subscription.
		if ok, serr := onvifEventSubscribePush(app, cam, camID); serr == nil && ok {
			lastLog = 0
			logIt(LINFO, "onvif motion via webhook subscribed", map[string]string{"camera": cam.Name, "url": redactNotifyURL(onvifNotifyURL(app))})
			// Renew before the subscription termination time, unsubscribing
			// the previous generation first so they don't accumulate.
			time.Sleep(10 * time.Minute)
			onvifEventUnsubscribe(camID)
			continue
		}
		// --- fallback: PullPoint subscription loop ---
		var uri string
		for {
			var err error
			uri, err = onvifEventSubscribe(cam)
			if err == nil {
				lastLog = 0
				logIt(LINFO, "onvif motion via pullpoint subscribed", map[string]string{"camera": cam.Name})
				break
			}
			if time.Now().Unix()-lastLog >= 60 {
				lastLog = time.Now().Unix()
				logIt(LWARN, "onvif motion: no working subscription (webhook+pullpoint both failed)", map[string]string{"camera": cam.Name, "error": err.Error(), "hint": "ensure onvif_notify_url is reachable over plain HTTP from the camera"})
			}
			time.Sleep(15 * time.Second)
		}
		// Poll messages from the pullpoint subscription.
		for {
			var raw, perr = onvifPullOnce(uri, cam)
			if perr != nil {
				logIt(LWARN, "onvif motion: pullpoint poll failed", map[string]string{"camera": cam.Name, "error": perr.Error()})
				break
			}
			if raw == nil {
				time.Sleep(2 * time.Second)
				continue
			}
			var dtype, active = onvifEventActive(raw)
			if dtype != "" {
				setCamActive(camID, active)
			} else if !active {
				// no recognized topic but explicit false -> clear active
				setCamActive(camID, false)
			}
			if active && dtype != "" {
				// Same backpressure as the webhook path: when the camera's
				// handlers are all busy, drop (and log) instead of queueing.
				if !onvifEventTryAcquire(camID) {
					logIt(LWARN, "onvif detection dropped (camera busy)", map[string]string{"camera": cam.Name, "type": dtype})
					continue
				}
				func() {
					defer onvifEventRelease(camID)
					if handleRawOnvifEvent(app, camID, cam.Name, dtype, recordingsSource(app, cam, camID)) {
						logIt(LINFO, "onvif detection", map[string]string{"camera": cam.Name, "type": dtype})
					}
				}()
			}
		}
		// Pullpoint broke: re-subscribe after a pause.
		time.Sleep(5 * time.Second)
	}
}

// onvifEventsStart launches one event poller per enabled camera whose effective
// detection source includes onvif.
func onvifEventsStart(app *App) {
	for i := range app.cfg.Cameras {
		var cam = app.cfg.Cameras[i]
		if !cam.Enabled || cam.Onvif.Url == "" {
			continue
		}
		if !sourceHas(camDetectionSource(app.cfg, &app.cfg.Cameras[i]), "onvif") {
			continue
		}
		var id = fmt.Sprintf("cam%d", i)
		go onvifEventLoop(app, &app.cfg.Cameras[i], id)
		logIt(LINFO, "onvif event poller started", map[string]string{"camera": cam.Name})
		if i > 0 {
			time.Sleep(1 * time.Second)
		} // stagger startups
	}
}

// notifyCams indexes cameras by stable camID (camN) for the ONVIF
// notification webhook. Names may collide or change, so they must not be
// used as the routing key.
var notifyCams = struct {
	sync.Mutex
	m map[string]int
}{m: map[string]int{}}

// onvifNotifyURL returns the externally-reachable URL for the ONVIF notify
// webhook (server port + optional configured override). The path token
// gating /onvif/notify is always appended when one is configured (explicit
// or derived from the UI password), including for explicit
// server.onvif_notify_url values — otherwise cameras would POST to the bare
// URL and handleOnvifNotify would reject them with 404, silently killing
// all detections (and downstream pushover/recording).
func onvifNotifyURL(app *App) string {
	if s := strings.TrimSpace(app.cfg.Server.OnvifNotifyURL); s != "" {
		s = strings.TrimSuffix(s, "/")
		if t := onvifNotifyToken(app); t != "" && !strings.HasSuffix(s, "/"+t) {
			s += "/" + t
		}
		return s
	}
	var ip = localPrivateIPv4()
	if ip == "" {
		ip = "127.0.0.1"
	}
	var u = fmt.Sprintf("http://%s:%d/onvif/notify", ip, app.cfg.Server.Port)
	if t := onvifNotifyToken(app); t != "" {
		u += "/" + t
	}
	return u
}

// isWeakPassword reports whether pw is too weak to seed the webhook secret
// from: empty, shorter than 8 chars, or a well-known default. Possession of
// such a password is nearly public, so a token derived from it would give a
// false sense of protection.
func isWeakPassword(pw string) bool {
	if len(pw) < 8 {
		return true
	}
	var l = strings.ToLower(pw)
	return l == "admin" || l == "changeme"
}

// notifyWeakWarnedMtx guards notifyWeakWarned (weak-password warning, once).
var notifyWeakWarnedMtx sync.Mutex
var notifyWeakWarned = false

// onvifNotifyToken returns the shared secret gating the /onvif/notify webhook
// (cameras cannot do HTTP Basic auth, so the secret travels in the path).
// Explicit server.onvif_notify_token wins; otherwise a stable token derived
// from the UI password is used so stock configs get a non-guessable,
// restart-stable secret with zero configuration. A weak or empty password
// (admin/changeme/<8 chars) must NOT seed the secret: an explicit
// server.onvif_notify_token is required instead (warned once), otherwise the
// webhook stays on the legacy open path (logged as a warning per request).
func onvifNotifyToken(app *App) string {
	if t := strings.TrimSpace(app.cfg.Server.OnvifNotifyToken); t != "" {
		return t
	}
	if pw := app.cfg.Server.Auth.Password; !isWeakPassword(pw) {
		var sum = sha256.Sum256([]byte("taponvr-onvif-notify:" + pw))
		return hex.EncodeToString(sum[:16])
	}
	notifyWeakWarnedMtx.Lock()
	var warn = !notifyWeakWarned
	notifyWeakWarned = true
	notifyWeakWarnedMtx.Unlock()
	if warn {
		logIt(LWARN, "onvif notify token disabled: weak or empty UI password", map[string]string{"hint": "set a strong server.auth.password (>=8 chars) or an explicit server.onvif_notify_token"})
	}
	return ""
}

// notifyRate limits ONVIF webhook POSTs per sender IP (fixed 1-minute window)
// so a misbehaving/forged sender cannot flood detections or fill the disk.
var notifyRate = struct {
	sync.Mutex
	hits map[string]notifyWindow
}{hits: map[string]notifyWindow{}}

type notifyWindow struct {
	count int
	start time.Time
}

// notifyAllowed reports whether one more webhook POST from ip fits the budget
// (120/min). Stale windows are purged on each check.
func notifyAllowed(ip string, now time.Time) bool {
	notifyRate.Lock()
	defer notifyRate.Unlock()
	for k, w := range notifyRate.hits {
		if now.Sub(w.start) >= time.Minute {
			delete(notifyRate.hits, k)
		}
	}
	var w = notifyRate.hits[ip]
	if now.Sub(w.start) >= time.Minute {
		w = notifyWindow{start: now}
	}
	w.count++
	notifyRate.hits[ip] = w
	return w.count <= 120
}

// localPrivateIPv4 returns the private IPv4 of the local interface that holds
// the default route (the primary LAN NIC, e.g. to reach the cameras), falling
// back to the first private IPv4 otherwise.
func localPrivateIPv4() string {
	// Preferred IP: the address used to reach the gateway. Use net.Dial to a
	// non-routable address on UDP; the OS picks the outgoing interface.
	if conn, err := net.Dial("udp", "192.168.1.1:80"); err == nil {
		defer conn.Close()
		if la, ok := conn.LocalAddr().(*net.UDPAddr); ok && la.IP != nil {
			if la.IP.To4() != nil {
				ip4 := la.IP.To4()
				if ip4[0] == 10 || (ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31) || (ip4[0] == 192 && ip4[1] == 168) {
					return ip4.String()
				}
			}
		}
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		var ipn, ok = a.(*net.IPNet)
		if !ok {
			continue
		}
		var ip4 = ipn.IP.To4()
		if ip4 == nil {
			continue
		}
		var b = ip4[0]
		if b == 10 || b == 127 || (b == 172 && ip4[1] >= 16 && ip4[1] <= 31) || (b == 192 && ip4[1] == 168) {
			return ip4.String()
		}
	}
	return ""
}

// onvifEventSubscribePush creates a WS-Base Notification Subscribe (push-based)
// subscription as a fallback when PullPoint is unsupported. A clean, minimal
// WS-Notification Subscribe request is sent with the taponvr notify webhook as
// the ConsumerReference. Returns true on success (notifications then arrive via
// the webhook).
func onvifEventSubscribePush(app *App, cam *CameraCfg, camID string) (bool, error) {
	dev := onvifGet(cam)
	if e := dev.cachedErr(); e != nil {
		return false, e
	}
	var ev = dev.eventsEndpoint()
	if ev == "" {
		return false, fmt.Errorf("no events endpoint")
	}
	var user, pass = cam.Onvif.Username, cam.Onvif.Password
	if strings.TrimSpace(camID) == "" {
		for i := range app.cfg.Cameras {
			if &app.cfg.Cameras[i] == cam {
				camID = fmt.Sprintf("cam%d", i)
				break
			}
		}
	}
	var cb = onvifNotifyURL(app) + "?camera=" + url.QueryEscape(camID)
	var term = time.Now().UTC().Add(time.Hour).Format("2006-01-02T15:04:05Z")
	var body = "<wsnt:Subscribe xmlns:wsnt=\"http://docs.oasis-open.org/wsn/b-2\"" +
		" xmlns:wsa=\"http://www.w3.org/2005/08/addressing\">" +
		"<wsnt:ConsumerReference><wsa:Address>" + xmlEscape(cb) + "</wsa:Address></wsnt:ConsumerReference>" +
		"<wsnt:InitialTerminationTime>" + term + "</wsnt:InitialTerminationTime>" +
		"</wsnt:Subscribe>"
	raw, err := onvifSoap(ev, "http://docs.oasis-open.org/wsn/bw-2/NotificationProducer/SubscribeRequest", body, user, pass)
	if err != nil {
		return false, err
	}
	// Success is a SubscribeResponse carrying a SubscriptionReference. (Some
	// Tapo firmwares reply without echoing our ConsumerReference back, so we
	// match on the response wrapper / SubscriptionReference instead.)
	var l = strings.ToLower(string(raw))
	if strings.Contains(l, "<wsnt:subscriberesponse") || strings.Contains(l, "subscriptionreference") {
		// Route by stable camID; resolve the index from it (fall back to the
		// camera pointer for safety).
		var idx = -1
		if n, serr := fmt.Sscanf(camID, "cam%d", &idx); serr != nil || n != 1 {
			idx = -1
		}
		if idx < 0 || idx >= len(app.cfg.Cameras) {
			for i := range app.cfg.Cameras {
				if &app.cfg.Cameras[i] == cam {
					idx = i
					break
				}
			}
		}
		notifyCams.Lock()
		if idx >= 0 {
			notifyCams.m[fmt.Sprintf("cam%d", idx)] = idx
		}
		notifyCams.Unlock()
		// Remember the SubscriptionReference address so the next renewal can
		// Unsubscribe first (otherwise subscriptions accumulate on the camera
		// every 10 min until its 1h termination time).
		if addr := parseSubscriptionAddr(string(raw)); addr != "" && idx >= 0 {
			notifySubs.Lock()
			notifySubs.m[fmt.Sprintf("cam%d", idx)] = notifySub{addr: addr, user: user, pass: pass}
			notifySubs.Unlock()
		}
		return true, nil
	}
	return false, fmt.Errorf("no SubscribeResponse: %s", strings.TrimSpace(string(raw)))
}

// notifySubs tracks live WS-Base subscriptions per camID for Unsubscribe.
var notifySubs = struct {
	sync.Mutex
	m map[string]notifySub
}{m: map[string]notifySub{}}

type notifySub struct {
	addr string
	user string
	pass string
}

// parseSubscriptionAddr extracts the SubscriptionReference address from a
// SubscribeResponse ("" when the camera omits it — renewal then behaves as
// before, without Unsubscribe).
func parseSubscriptionAddr(resp string) string {
	var low = strings.ToLower(resp)
	var i = strings.Index(low, "subscriptionreference")
	if i < 0 {
		return ""
	}
	// Skip the rest of the <...SubscriptionReference ...> start tag: it may
	// declare a namespace whose URI contains "addressing", which would
	// otherwise match before the real <Address> element and yield "".
	var gt0 = strings.IndexByte(resp[i:], '>')
	if gt0 < 0 {
		return ""
	}
	var seg = resp[i+gt0+1:]
	var ai = strings.Index(strings.ToLower(seg), "address")
	if ai < 0 {
		return ""
	}
	seg = seg[ai:]
	var gt = strings.Index(seg, ">")
	if gt < 0 {
		return ""
	}
	seg = seg[gt+1:]
	var lt = strings.Index(seg, "<")
	if lt < 0 {
		return ""
	}
	var addr = strings.TrimSpace(seg[:lt])
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		return ""
	}
	return addr
}

// onvifEventUnsubscribe tears down a previous WS-Base subscription (best
// effort: failures only log, renewal continues regardless).
func onvifEventUnsubscribe(camID string) {
	notifySubs.Lock()
	sub, ok := notifySubs.m[camID]
	if ok {
		delete(notifySubs.m, camID)
	}
	notifySubs.Unlock()
	if !ok || sub.addr == "" {
		return
	}
	var body = "<wsnt:Unsubscribe xmlns:wsnt=\"http://docs.oasis-open.org/wsn/b-2\"/>"
	if _, err := onvifSoap(sub.addr, "http://docs.oasis-open.org/wsn/bw-2/SubscriptionManager/UnsubscribeRequest", body, sub.user, sub.pass); err != nil {
		logIt(LDEBUG, "onvif unsubscribe failed", map[string]string{"camera": camID, "error": err.Error()})
		return
	}
	logIt(LDEBUG, "onvif unsubscribed", map[string]string{"camera": camID})
}

// onvifEventSem bounds concurrent handleRawOnvifEvent executions per camera:
// the webhook must answer fast, and unbounded `go handleRawOnvifEvent`
// spawns would pile up slow snapshot/group work without limit on event
// floods (forged or real). Each camera gets 8 slots; excess events are
// dropped with a 429/log instead of queueing.
var onvifEventSemMtx sync.Mutex
var onvifEventSem = map[string]chan struct{}{}

// onvifEventSlot returns the per-camera event semaphore (buffered, 8).
func onvifEventSlot(camID string) chan struct{} {
	onvifEventSemMtx.Lock()
	defer onvifEventSemMtx.Unlock()
	if ch, ok := onvifEventSem[camID]; ok {
		return ch
	}
	var ch = make(chan struct{}, 8)
	onvifEventSem[camID] = ch
	return ch
}

// onvifEventTryAcquire takes one per-camera event slot, reporting false when
// all 8 are busy (caller drops the event instead of queueing).
func onvifEventTryAcquire(camID string) bool {
	select {
	case onvifEventSlot(camID) <- struct{}{}:
		return true
	default:
		return false
	}
}

// onvifEventRelease frees one per-camera event slot.
func onvifEventRelease(camID string) {
	<-onvifEventSlot(camID)
}

// notifyOpenWarned logs the open-webhook warning only once per process.
var notifyOpenWarnedMtx sync.Mutex
var notifyOpenWarned = false

// handleOnvifNotify receives ONVIF push notifications from cameras and records
// a detection when the notification matches a known type. Cameras cannot do
// HTTP Basic auth, so the endpoint is gated by a shared-secret path token
// (/onvif/notify/<token>) plus a per-IP rate limit and a 1MB body cap.
func handleOnvifNotify(app *App, w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, "{\"error\":\"method not allowed\"}")
		return
	}
	if tok := onvifNotifyToken(app); tok != "" {
		var got = strings.Trim(strings.TrimPrefix(r.URL.Path, "/onvif/notify"), "/")
		if subtle.ConstantTimeCompare([]byte(got), []byte(tok)) != 1 {
			writeJSON(w, 404, "{\"error\":\"not found\"}")
			return
		}
	} else {
		notifyOpenWarnedMtx.Lock()
		var warn = !notifyOpenWarned
		notifyOpenWarned = true
		notifyOpenWarnedMtx.Unlock()
		if warn {
			logIt(LWARN, "onvif notify webhook is open (no password set)", map[string]string{"hint": "set server.auth.password or server.onvif_notify_token"})
		}
	}
	var ip, _, serr = net.SplitHostPort(r.RemoteAddr)
	if serr != nil || ip == "" {
		ip = r.RemoteAddr
		if ip == "" {
			ip = "unknown"
		}
	}
	if !notifyAllowed(ip, time.Now()) {
		writeJSON(w, 429, "{\"error\":\"rate limited\"}")
		return
	}
	// Cap the body BEFORE Close is deferred: MaxBytesReader must wrap the
	// original body, and the wrapped body is what gets closed.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	var body, berr = io.ReadAll(r.Body)
	if berr != nil {
		writeJSON(w, 413, "{\"error\":\"body too large\"}")
		return
	}
	var camParam = r.URL.Query().Get("camera")
	var idx = -1
	notifyCams.Lock()
	if i, ok := notifyCams.m[camParam]; ok {
		idx = i
	}
	notifyCams.Unlock()
	if idx < 0 {
		// Legacy fallback: subscriptions created before the camID routing
		// change carry ?camera=<name>. Accept them until re-subscription
		// (10 min renew) moves every camera to ?camera=<camID>.
		for i := range app.cfg.Cameras {
			if fmt.Sprintf("cam%d", i) == camParam || app.cfg.Cameras[i].Name == camParam {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		// Not subscribed yet; log at low severity only.
		writeJSON(w, 200, "{\"ok\":false}")
		return
	}
	var dtype, active = onvifEventActive(body)
	var camID = fmt.Sprintf("cam%d", idx)
	var camName = app.cfg.Cameras[idx].Name
	if dtype != "" {
		setCamActive(camID, active)
	} else if !active {
		setCamActive(camID, false)
	}
	if dtype != "" && active {
		var cam = &app.cfg.Cameras[idx]
		var src = recordingsSource(app, cam, camID)
		// Record off the request goroutine: collectDetection grabs snapshots
		// (several seconds) and would make the camera's webhook POST time out
		// and retry, producing duplicate detections. The camera only needs a
		// fast 200. The per-IP rate limit above plus the per-camera semaphore
		// below bound how many handlers can pile up; excess events are
		// dropped with a 429 instead of spawning unbounded goroutines.
		if !onvifEventTryAcquire(camID) {
			logIt(LWARN, "onvif detection dropped (camera busy)", map[string]string{"camera": camName, "type": dtype})
			writeJSON(w, 429, "{\"error\":\"camera busy\"}")
			return
		}
		go func(camID string, camName string, dtype string, src string) {
			defer onvifEventRelease(camID)
			if handleRawOnvifEvent(app, camID, camName, dtype, src) {
				logIt(LINFO, "onvif detection", map[string]string{"camera": camName, "type": dtype})
			}
		}(camID, camName, dtype, src)
	}
	writeJSON(w, 200, "{\"ok\":true}")
}
