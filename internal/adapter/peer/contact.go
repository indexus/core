package peer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	indexus "github.com/indexus/go-indexus-core/indexus"
	"github.com/indexus/go-indexus-core/internal/adapter/meshhttp"
	"github.com/indexus/go-indexus-core/internal/auth"
	"github.com/indexus/go-indexus-core/internal/wire"
)

// Share the mesh's lazy transport hook: H3 is selected before the first request,
// after main installs it. Five seconds leaves room for the two-second QUIC
// handshake and a TCP/TLS fallback; this is independent of mesh RPC deadlines.
var HttpClient = &http.Client{Timeout: 5 * time.Second, Transport: meshhttp.NewClient().HTTP.Transport}

var OutboundBearer string

func withAuth(req *http.Request) {
	req.Header.Set("Accept-Encoding", "identity")
	if OutboundBearer != "" {
		req.Header.Set("Authorization", "Bearer "+OutboundBearer)
	}
}

type Contact struct {
	scheme string
	name   string
	id     []byte
	ips    map[string]any
	ip     string
	port   int
	cert   *auth.NodeCert
}

func (c *Contact) MarshalJSON() ([]byte, error) {
	type Alias struct {
		Scheme string         `json:"scheme,omitempty"`
		Name   string         `json:"name"`
		IPs    map[string]any `json:"ips"`
		IP     string         `json:"ip"`
		Port   int            `json:"port"`
		Cert   *auth.NodeCert `json:"cert,omitempty"`
	}
	return json.Marshal(&Alias{
		Scheme: c.scheme,
		Name:   c.name,
		IPs:    c.ips,
		IP:     c.ip,
		Port:   c.port,
		Cert:   c.cert,
	})
}

func (c *Contact) UnmarshalJSON(data []byte) error {
	type Alias struct {
		Scheme string         `json:"scheme,omitempty"`
		Name   string         `json:"name"`
		IPs    map[string]any `json:"ips"`
		IP     string         `json:"ip"`
		Port   int            `json:"port"`
		Cert   *auth.NodeCert `json:"cert,omitempty"`
	}
	aux := &Alias{}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.Scheme != "" && aux.Scheme != "http" && aux.Scheme != "https" {
		return fmt.Errorf("invalid peer scheme %q", aux.Scheme)
	}
	c.scheme = aux.Scheme
	c.name = aux.Name
	c.ips = aux.IPs
	c.ip = aux.IP
	c.port = aux.Port
	c.cert = aux.Cert
	id, err := indexus.DecodeVnodeID(aux.Name)
	if err != nil {
		return fmt.Errorf("contact: %w", err)
	}
	c.id = id
	return nil
}

func NewContact(name string, ips map[string]any, port int) indexus.Contact {
	id, _ := indexus.DecodeVnodeID(name) // nil for a malformed name: never routed
	c := &Contact{
		name: name,
		id:   id,
		ips:  ips,
		port: port,
	}

	for ip := range ips {
		if ip != "" {
			c.ip = ip
			break
		}
	}
	return c
}

// NewTLSContact is the contact factory of a TLS node. Contacts learned from
// canonical mesh endpoints may override it with their explicit scheme.
func NewTLSContact(name string, ips map[string]any, port int) indexus.Contact {
	c := NewContact(name, ips, port).(*Contact)
	c.scheme = "https"
	return c
}

func (c *Contact) SetScheme(scheme string) { c.scheme = scheme }
func (c *Contact) Endpoint() string        { return c.endpoint("") }

func (c *Contact) Cert() *auth.NodeCert { return c.cert }

func (c *Contact) SetCert(cert *auth.NodeCert) { c.cert = cert }

func (c *Contact) ID() []byte {
	return c.id
}

func (c *Contact) Name() string {
	return c.name
}

func (c *Contact) IPs() map[string]any {
	return c.ips
}

func (c *Contact) Port() int {
	return c.port
}

func (c *Contact) IP() string {
	return c.ip
}

func (c *Contact) Host() string {
	ip := c.ip
	if ip == "" {
		for candidate := range c.ips {
			if candidate != "" {
				ip = candidate
				break
			}
		}
	}
	return fmt.Sprintf("%s@%s|%d", c.name, ip, c.port)
}

func (c *Contact) Ping(origin indexus.Contact) (indexus.Contact, error) {
	ips := c.ips
	if len(ips) == 0 && c.ip != "" {
		ips = map[string]any{c.ip: nil}
	}
	for ip := range ips {
		if ip == "" {
			continue
		}
		contact, err := c.ping(origin, ip)
		if err != nil {
			continue
		}
		return contact, nil
	}
	return nil, fmt.Errorf("no hosts found from ips and port provided")
}

func (c *Contact) ping(origin indexus.Contact, ip string) (indexus.Contact, error) {
	targetURL := c.endpointAt(ip, "/ping")
	originPeer := &Contact{
		scheme: c.scheme,
		name:   origin.Name(),
		ips:    origin.IPs(),
		port:   origin.Port(),
	}
	if endpoint, ok := origin.(interface{ Endpoint() string }); ok {
		if u, err := url.Parse(endpoint.Endpoint()); err == nil {
			originPeer.scheme = u.Scheme
		}
	}
	if oc, ok := origin.(interface{ Cert() *auth.NodeCert }); ok {
		originPeer.cert = oc.Cert()
	}
	reqBody := struct {
		Origin *Contact `json:"origin"`
	}{
		Origin: originPeer,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	withAuth(req)

	resp, err := HttpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ping %s: unexpected status %d", targetURL, resp.StatusCode)
	}

	var respBody struct {
		Contact *Contact `json:"contact"`
	}
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&respBody); err != nil {
		return nil, err
	}

	contact := respBody.Contact
	if contact == nil {
		return nil, fmt.Errorf("ping %s: missing contact", targetURL)
	}
	if contact.scheme == "" {
		contact.scheme = c.scheme
	}
	if contact.ips == nil {
		contact.ips = map[string]any{}
	}
	contact.ips[ip] = nil
	contact.ip = ip

	return contact, nil
}

func (c *Contact) Neighbors(origin indexus.Peer) ([]indexus.Contact, error) {
	var body struct {
		Neighbors []*Contact `json:"neighbors"`
	}
	if err := c.readDiscovery("neighbors", origin, &body); err != nil {
		return nil, err
	}

	for _, neighbor := range body.Neighbors {
		if neighbor.scheme == "" {
			neighbor.scheme = c.scheme
		}
		if neighbor.Name() == c.Name() {
			neighbor.ip = c.ip
		}
	}

	return indexus.ConvertToContactSlice(body.Neighbors), nil
}

func (c *Contact) Random(origin indexus.Peer) (indexus.Contact, error) {
	// Handler writes {"random": {...}}; a missing/null body must be a true
	// nil interface, not a typed (*Contact)(nil), or callers panic on .Name().
	var body struct {
		Random *Contact `json:"random"`
	}
	if err := c.readDiscovery("random", origin, &body); err != nil {
		return nil, err
	}
	if body.Random == nil {
		return nil, nil
	}
	if body.Random.scheme == "" {
		body.Random.scheme = c.scheme
	}
	return body.Random, nil
}

func (c *Contact) Get(collection string, location string, deep bool, via indexus.Visited, refresh bool) (indexus.Contact, *indexus.Set, error) {
	// Inter-node reads use the same /sets protocol as browser clients so the
	// two paths cannot diverge. A peer is asked for float64 metrics: a relayed
	// zone then keeps its count and every metric as the owner holds them,
	// whatever the number of hops.
	rawURL := c.endpoint(fmt.Sprintf("/sets?collection=%s&location=%s&deep=%t&metrics=f64",
		url.QueryEscape(collection), url.QueryEscape(location), deep))
	if s := via.String(); s != "" {
		rawURL += "&via=" + url.QueryEscape(s)
	}
	if refresh {
		rawURL += "&refresh=true"
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	withAuth(req)
	resp, err := HttpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("error code: %d", resp.StatusCode)
	}

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, nil, err
	}
	payload := buf.Bytes()

	contentType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	if strings.TrimSpace(contentType) != wire.SetsBlockContentType {
		return nil, nil, fmt.Errorf("sets: content type %q, not IXB2", contentType)
	}
	block, err := wire.DecodeSetsBlock(payload)
	if err != nil {
		return nil, nil, err
	}
	redirects, entries := block.Redirects, block.Entries()

	var contact indexus.Contact
	for _, r := range redirects {
		if r.Location != location || r.Name == "" || r.Port <= 0 {
			continue
		}
		next := NewContact(r.Name, map[string]any{r.IP: nil}, r.Port).(*Contact)
		next.scheme = c.scheme
		contact = next
		break
	}
	if len(entries) == 0 {
		return contact, nil, nil
	}
	set := wire.NewSet()
	for key, value := range entries {
		set.Put(key, value)
	}
	return contact, set, nil
}

func (c *Contact) New(item *indexus.Item, root string, via indexus.Visited) error {
	return c.writeItem("/item", item, root, via)
}
func (c *Contact) Delete(item *indexus.Item, root string, via indexus.Visited) error {
	return c.writeItem("/item/delete", item, root, via)
}

func (c *Contact) writeItem(path string, item *indexus.Item, root string, via indexus.Visited) error {
	targetURL := c.endpoint(path)
	body := struct {
		Item *indexus.Item `json:"item"`
		Root string        `json:"root"`
		Via  string        `json:"via,omitempty"`
	}{
		Item: item,
		Root: root,
		Via:  via.String(),
	}

	jsonData, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	if path == "/item" {
		req.Header.Set(indexus.HandoffHeader, "1")
	}
	withAuth(req)

	resp, err := HttpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		return indexus.ErrPeerBusy
	}
	if resp.StatusCode != http.StatusCreated && (path != "/item/delete" || resp.StatusCode != http.StatusOK) {
		return fmt.Errorf("error code: %d", resp.StatusCode)
	}

	return nil
}

// endpoint preserves the peer protocol's literal IP and query formatting.
func (c *Contact) endpoint(path string) string {
	return c.endpointAt(c.ip, path)
}

func (c *Contact) endpointAt(ip, path string) string {
	scheme := c.scheme
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + net.JoinHostPort(strings.Trim(ip, "[]"), strconv.Itoa(c.port)) + path
}

func (c *Contact) readDiscovery(path string, origin indexus.Peer, out any) error {
	url := c.endpoint("/" + path + "?origin=" + origin.Name())

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("error creating request: %s", err.Error())
	}
	withAuth(req)

	resp, err := HttpClient.Do(req)
	if err != nil {
		return fmt.Errorf("error making request: %s", err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("error code: %d", resp.StatusCode)
	}

	return json.NewDecoder(resp.Body).Decode(out)
}
