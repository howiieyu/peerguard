package peermap

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	cmap "github.com/orcaman/concurrent-map/v2"
	"github.com/rkonfj/peerguard/peer"
	"github.com/rkonfj/peerguard/peermap/auth"
	"github.com/rkonfj/peerguard/peermap/oidc"
	"golang.org/x/time/rate"
)

type Network struct {
	ID         string `json:"id"`
	PeersCount int    `json:"peersCount"`
}

type Peer struct {
	peerMap        *PeerMap
	networkContext *networkContext
	metadata       peer.Metadata
	conn           *websocket.Conn
	networkID      peer.NetworkID
	id             peer.PeerID
	nonce          byte
	wMut           sync.Mutex
}

func (p *Peer) write(b []byte) error {
	return p.writeWS(websocket.BinaryMessage, b)
}

func (p *Peer) writeWS(messageType int, b []byte) error {
	if p.networkContext.ratelimiter != nil {
		p.networkContext.ratelimiter.WaitN(context.Background(), len(b))
	}
	p.wMut.Lock()
	defer p.wMut.Unlock()
	return p.conn.WriteMessage(messageType, b)
}

func (p *Peer) close() error {
	if ctx, ok := p.peerMap.networkMap.Get(string(p.networkID)); ok {
		ctx.Remove(string(p.id))
	}
	_ = p.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(2*time.Second))
	return p.conn.Close()
}

func (p *Peer) String() string {
	metadata := url.Values{}
	metadata.Add("alias1", p.metadata.Alias1)
	metadata.Add("alias2", p.metadata.Alias2)
	for k, v := range p.metadata.Extra {
		b, _ := json.Marshal(v)
		metadata.Add(k, string(b))
	}
	return (&url.URL{
		Scheme:   "pg",
		Host:     string(p.id),
		RawQuery: metadata.Encode(),
	}).String()
}

func (p *Peer) Start() {
	go p.readMessageLoope()
	go p.keepalive()
	if p.metadata.SilenceMode {
		return
	}

	ctx, _ := p.peerMap.networkMap.Get(string(p.networkID))
	for target := range ctx.IterBuffered() {
		if target.Key == string(p.id) {
			continue
		}

		if target.Val.metadata.SilenceMode {
			continue
		}
		p.leadDisco(target.Val)
	}
}

func (p *Peer) leadDisco(target *Peer) {
	myMeta := p.metadata.MustMarshalJSON()
	b := make([]byte, 2+len(p.id)+len(myMeta))
	b[0] = peer.CONTROL_NEW_PEER
	b[1] = p.id.Len()
	copy(b[2:], p.id.Bytes())
	copy(b[len(p.id)+2:], myMeta)
	for i, v := range b {
		b[i] = v ^ target.nonce
	}
	target.write(b)

	peerMeta := target.metadata.MustMarshalJSON()
	b1 := make([]byte, 2+len(target.id)+len(peerMeta))
	b1[0] = peer.CONTROL_NEW_PEER
	b1[1] = target.id.Len()
	copy(b1[2:], target.id.Bytes())
	copy(b1[len(target.id)+2:], peerMeta)
	for i, v := range b1 {
		b1[i] = v ^ p.nonce
	}
	p.write(b1)
}

func (p *Peer) readMessageLoope() {
	for {
		mt, b, err := p.conn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err,
				websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				slog.Error(err.Error())
			}
			p.close()
			return
		}
		if p.networkContext.ratelimiter != nil {
			p.networkContext.ratelimiter.WaitN(context.Background(), len(b))
		}
		switch mt {
		case websocket.PingMessage:
			p.conn.WriteMessage(websocket.PongMessage, nil)
			continue
		case websocket.BinaryMessage:
		default:
			continue
		}
		for i, v := range b {
			b[i] = v ^ p.nonce
		}
		tgtPeerID := peer.PeerID(b[2 : b[1]+2])
		slog.Debug("PeerEvent", "op", b[0], "from", p.id, "to", tgtPeerID)
		tgtPeer, err := p.peerMap.FindPeer(p.networkID, tgtPeerID)
		if err != nil {
			slog.Debug("FindPeer failed", "detail", err)
			continue
		}
		switch b[0] {
		case peer.CONTROL_LEAD_DISCO:
			p.leadDisco(tgtPeer)
		default:
			data := b[b[1]+2:]
			bb := make([]byte, 2+len(p.id)+len(data))
			bb[0] = b[0]
			bb[1] = p.id.Len()
			copy(bb[2:p.id.Len()+2], p.id.Bytes())
			copy(bb[p.id.Len()+2:], data)
			for i, v := range bb {
				bb[i] = v ^ tgtPeer.nonce
			}
			_ = tgtPeer.write(bb)
		}
	}
}

func (p *Peer) keepalive() {
	for {
		time.Sleep(10 * time.Second)
		if err := p.writeWS(websocket.PingMessage, nil); err != nil {
			break
		}
	}
}

type networkContext struct {
	cmap.ConcurrentMap[string, *Peer]
	ratelimiter *rate.Limiter
}

type PeerMap struct {
	httpServer    *http.Server
	wsUpgrader    *websocket.Upgrader
	networkMap    cmap.ConcurrentMap[string, *networkContext]
	cfg           Config
	authenticator auth.Authenticator
}

func (pm *PeerMap) FindPeer(networkID peer.NetworkID, peerID peer.PeerID) (*Peer, error) {
	if ctx, ok := pm.networkMap.Get(string(networkID)); ok {
		if peer, ok := ctx.Get(string(peerID)); ok {
			return peer, nil
		}
	}
	return nil, fmt.Errorf("peer(%s/%s) not found", networkID, peerID)
}

func (pm *PeerMap) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		fmt.Println("Graceful shutdown")
		pm.close()
	}()
	slog.Info("Serving for http now", "listen", pm.cfg.Listen)
	return pm.httpServer.ListenAndServe()
}

func (pm *PeerMap) close() {
	pm.httpServer.Shutdown(context.Background())
}

func New(cfg Config) (*PeerMap, error) {
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	pm := PeerMap{
		httpServer:    &http.Server{Handler: mux, Addr: cfg.Listen},
		wsUpgrader:    &websocket.Upgrader{},
		networkMap:    cmap.New[*networkContext](),
		authenticator: auth.NewAuthenticator(cfg.SecretKey),
		cfg:           cfg,
	}
	mux.HandleFunc("/", pm.handleWebsocket)
	mux.HandleFunc("/networks", pm.handleQueryNetworks)
	mux.HandleFunc("/peers", pm.handleQueryNetworkPeers)
	mux.HandleFunc("/network/token", oidc.HandleNotifyToken)
	mux.HandleFunc("/oidc/", oidc.RedirectAuthURL)
	mux.HandleFunc("/oidc/authorize/", pm.handleOIDCAuthorize)
	return &pm, nil
}

func (pm *PeerMap) handleQueryNetworks(w http.ResponseWriter, r *http.Request) {
	items := pm.networkMap.Items()
	networks := make([]Network, 0, len(items))
	for k, v := range items {
		networks = append(networks, Network{
			ID:         k,
			PeersCount: v.Count(),
		})
	}
	json.NewEncoder(w).Encode(networks)
}

func (pm *PeerMap) handleQueryNetworkPeers(w http.ResponseWriter, r *http.Request) {
	networkID, err := pm.authenticator.VerifyToken(r.Header.Get("X-Network"))
	if err != nil {
		slog.Debug("Authenticate failed", "err", err, "network", r.Header.Get("X-Network"))
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if networkContext, ok := pm.networkMap.Get(networkID); ok {
		items := networkContext.Items()
		peers := make([]string, 0, len(items))
		for _, v := range items {
			peers = append(peers, v.String())
		}
		json.NewEncoder(w).Encode(peers)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (pm *PeerMap) handleOIDCAuthorize(w http.ResponseWriter, r *http.Request) {
	providerName := path.Base(r.URL.Path)
	provider, ok := oidc.Provider(providerName)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	email, _, err := provider.UserInfo(r.URL.Query().Get("code"))
	if err != nil {
		slog.Error("OIDC get userInfo error", "err", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	networkB := md5.Sum([]byte(email))
	network := base64.URLEncoding.EncodeToString(networkB[:])
	token, err := auth.NewAuthenticator(pm.cfg.SecretKey).GenerateToken(network, 2*24*time.Hour)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	err = oidc.NotifyToken(r.URL.Query().Get("state"), oidc.NetworkSecret{
		Network: network,
		Secret:  peer.NetworkSecret(token),
		Expire:  time.Now().Add(2*24*time.Hour - 10*time.Second),
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Write([]byte("ok"))
}

func (pm *PeerMap) handleWebsocket(w http.ResponseWriter, r *http.Request) {
	networkID := r.Header.Get("X-Network")
	if len(networkID) > 0 {
		pm.handlePeerPacketConnect(w, r)
		return
	}
	w.WriteHeader(http.StatusForbidden)
}

func (pm *PeerMap) handlePeerPacketConnect(w http.ResponseWriter, r *http.Request) {
	networkID, err := pm.authenticator.VerifyToken(r.Header.Get("X-Network"))
	if err != nil {
		slog.Debug("Authenticate failed", "err", err, "network", r.Header.Get("X-Network"))
		w.WriteHeader(http.StatusForbidden)
		return
	}

	peerID := r.Header.Get("X-PeerID")

	nonce := peer.MustParseNonce(r.Header.Get("X-Nonce"))

	if !pm.networkMap.Has(networkID) {
		var rateLimiter *rate.Limiter
		if pm.cfg.RateLimiter != nil {
			if pm.cfg.RateLimiter.Limit > 0 {
				rateLimiter = rate.NewLimiter(rate.Limit(pm.cfg.RateLimiter.Limit), pm.cfg.RateLimiter.Burst)
			}
		}
		pm.networkMap.SetIfAbsent(networkID, &networkContext{
			ConcurrentMap: cmap.New[*Peer](),
			ratelimiter:   rateLimiter,
		})
	}

	networkCtx, _ := pm.networkMap.Get(networkID)
	peer := Peer{
		peerMap:        pm,
		networkContext: networkCtx,
		networkID:      peer.NetworkID(networkID),
		id:             peer.PeerID(peerID),
		nonce:          nonce,
		metadata:       peer.Metadata{},
	}

	metadata := r.Header.Get("X-Metadata")
	if len(metadata) > 0 {
		b, err := base64.StdEncoding.DecodeString(metadata)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		json.Unmarshal(b, &peer.metadata)
	}

	if ok := networkCtx.SetIfAbsent(peerID, &peer); !ok {
		slog.Debug("Address is already in used", "addr", peerID)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	upgradeHeader := http.Header{}
	upgradeHeader.Set("X-Nonce", r.Header.Get("X-Nonce"))
	stuns, _ := json.Marshal(pm.cfg.STUNs)
	upgradeHeader.Set("X-STUNs", base64.StdEncoding.EncodeToString(stuns))
	wsConn, err := pm.wsUpgrader.Upgrade(w, r, upgradeHeader)
	if err != nil {
		slog.Error(err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	peer.conn = wsConn
	peer.Start()
}
