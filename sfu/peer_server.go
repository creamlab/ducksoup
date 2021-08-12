package sfu

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
)

type peerServer struct {
	sync.Mutex
	userId     string
	room       *trialRoom
	join       joinPayload
	pc         *peerConn
	ws         *wsConn
	audioTrack *localTrack
	videoTrack *localTrack
	closed     bool
	closedCh   chan struct{}
}

func newPeerServer(
	join joinPayload,
	room *trialRoom,
	pc *peerConn,
	ws *wsConn) *peerServer {
	ps := &peerServer{
		userId:   join.UserId,
		room:     room,
		join:     join,
		pc:       pc,
		ws:       ws,
		closed:   false,
		closedCh: make(chan struct{}),
	}

	// connect components for further communication
	room.connectPeerServer(ps) // also triggers signaling
	pc.connectPeerServer(ps)

	return ps
}

func (ps *peerServer) setLocalTrack(kind string, outputTrack *localTrack) {
	if kind == "audio" {
		ps.audioTrack = outputTrack
	} else if kind == "video" {
		ps.videoTrack = outputTrack
	}
}

func (ps *peerServer) close() {
	ps.Lock()
	defer ps.Unlock()

	if !ps.closed {
		log.Printf("[ps user#%s] closing\n", ps.userId)
		// ps.closed check ensure closedCh is not closed twice
		ps.closed = true

		// listened by localTracks
		close(ps.closedCh)
		// clean up bound components
		ps.room.disconnectUser(ps.userId)
		ps.pc.Close()
		ps.ws.Close()
	}
}

func (ps *peerServer) loop() {
	var m messageIn

	// sends "ending" message before rooms does end
	go func() {
		<-ps.room.waitForAllCh
		<-time.After(time.Duration(ps.room.endingDelay()) * time.Second)
		log.Printf("[ps user#%s] ending message sent\n", ps.userId)
		ps.ws.send("ending")
	}()

	for {
		select {
		case <-ps.room.endCh:
			ps.close()
			return
		default:
			err := ps.ws.ReadJSON(&m)

			if err != nil {
				ps.close()
				if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure) {
					log.Printf("[ps user#%s][error] reading JSON: %v\n", ps.userId, err)
				}
				return
			}

			switch m.Kind {
			case "candidate":
				candidate := webrtc.ICECandidateInit{}
				if err := json.Unmarshal([]byte(m.Payload), &candidate); err != nil {
					log.Printf("[ps user#%s][error] unmarshal candidate: %v\n", ps.userId, err)
					return
				}

				if err := ps.pc.AddICECandidate(candidate); err != nil {
					log.Printf("[ps user#%s][error] add candidate: %v\n", ps.userId, err)
					return
				}
			case "answer":
				answer := webrtc.SessionDescription{}
				if err := json.Unmarshal([]byte(m.Payload), &answer); err != nil {
					log.Printf("[ps user#%s][error] unmarshal answer: %v\n", ps.userId, err)
					return
				}

				if err := ps.pc.SetRemoteDescription(answer); err != nil {
					log.Printf("[ps user#%s][error] SetRemoteDescription: %v\n", ps.userId, err)
					return
				}
			case "control":
				payload := controlPayload{}
				if err := json.Unmarshal([]byte(m.Payload), &payload); err != nil {
					log.Printf("[ps user#%s][error] unmarshal control: %v\n", ps.userId, err)
				} else {
					go func() {
						if payload.Kind == "audio" && ps.audioTrack != nil {
							ps.audioTrack.controlFx(payload)
						} else if ps.videoTrack != nil {
							ps.videoTrack.controlFx(payload)
						}
					}()
				}
			}
		}
	}
}

// API

// handle incoming websockets
func RunPeerServer(origin string, unsafeConn *websocket.Conn) {

	ws := newWsConn(unsafeConn)
	defer ws.Close()

	// first message must be a join request
	joinPayload, err := ws.readJoin(origin)
	if err != nil {
		ws.send("error-join")
		log.Printf("[ps user unknown][error] join payload corrupted: %v\n", err)
		return
	}
	userId := joinPayload.UserId

	// used to log info with user id
	ws.setUserId(userId)

	room, err := joinRoom(joinPayload)
	if err != nil {
		// joinRoom err is meaningful to client
		ws.send(fmt.Sprintf("error-%s", err))
		log.Printf("[ps user#%s][error] join failed: %s", userId, err)
		return
	}

	pc, err := newPeerConn(joinPayload, room, ws)
	if err != nil {
		ws.send("error-peer-connection")
		log.Printf("[ps user#%s][error] pc creation failed: %s", userId, err)
		return
	}

	ps := newPeerServer(joinPayload, room, pc, ws)

	ps.loop() // blocking
}
