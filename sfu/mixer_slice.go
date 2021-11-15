package sfu

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/creamlab/ducksoup/gst"
	_ "github.com/creamlab/ducksoup/helpers" // rely on helpers logger init side-effect
	"github.com/creamlab/ducksoup/sequencing"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	defaultInterpolatorStep = 30
	maxInterpolatorDuration = 5000
	encoderPeriod           = 1000
	statsPeriod             = 3000
	logPeriod               = 7300
)

type mixerSlice struct {
	sync.Mutex
	fromPs *peerServer
	kind   string
	// webrtc
	input    *webrtc.TrackRemote
	output   *webrtc.TrackLocalStaticRTP
	receiver *webrtc.RTPReceiver
	// processing
	pipeline          *gst.Pipeline
	interpolatorIndex map[string]*sequencing.LinearInterpolator
	// controller
	senderControllerIndex map[string]*senderController // per user id
	optimalBitrate        uint64
	encoderTicker         *time.Ticker
	// stats
	statsTicker   *time.Ticker
	logTicker     *time.Ticker
	lastStats     time.Time
	inputBits     int64
	outputBits    int64
	inputBitrate  int64
	outputBitrate int64
	// status
	endCh chan struct{} // stop processing when track is removed
	// log
	logger zerolog.Logger
}

// helpers

func minUint64Slice(v []uint64) (min uint64) {
	if len(v) > 0 {
		min = v[0]
	}
	for i := 1; i < len(v); i++ {
		if v[i] < min {
			min = v[i]
		}
	}
	return
}

func newMixerSlice(ps *peerServer, remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) (slice *mixerSlice, err error) {
	// create a new mixerSlice with:
	// - the same codec format as the incoming/remote one
	// - a unique server-side trackId, but won't be reused in the browser, see https://developer.mozilla.org/en-US/docs/Web/API/MediaStreamTrack/id
	// - a streamId shared among peerServer tracks (audio/video)
	// newId := uuid.New().String()
	kind := remoteTrack.Kind().String()
	if kind != "audio" && kind != "video" {
		return nil, errors.New("invalid kind")
	}

	newId := remoteTrack.ID()
	localTrack, err := webrtc.NewTrackLocalStaticRTP(remoteTrack.Codec().RTPCodecCapability, newId, ps.streamId)

	if err != nil {
		return
	}

	logger := log.With().
		Str("room", ps.roomId).
		Str("fromUser", ps.userId).
		Logger()

	slice = &mixerSlice{
		fromPs: ps,
		kind:   kind,
		// webrtc
		input:    remoteTrack,
		output:   localTrack,
		receiver: receiver, // TODO read RTCP?
		// processing
		pipeline:          ps.pipeline,
		interpolatorIndex: make(map[string]*sequencing.LinearInterpolator),
		// controller
		senderControllerIndex: map[string]*senderController{},
		encoderTicker:         time.NewTicker(encoderPeriod * time.Millisecond),
		// stats
		statsTicker: time.NewTicker(statsPeriod * time.Millisecond),
		logTicker:   time.NewTicker(logPeriod * time.Millisecond),
		lastStats:   time.Now(),
		// status
		endCh:  make(chan struct{}),
		logger: logger,
	}
	return
}

// Same ID as output track
func (s *mixerSlice) ID() string {
	return s.output.ID()
}

func (s *mixerSlice) stop() {
	s.pipeline.Stop()
	s.statsTicker.Stop()
	s.encoderTicker.Stop()
	s.logTicker.Stop()
	close(s.endCh)
}

func (s *mixerSlice) addSender(sender *webrtc.RTPSender, toUserId string) {
	params := sender.GetParameters()

	if len(params.Encodings) == 1 {
		sc := newSenderController(sender, s, toUserId)
		s.Lock()
		s.senderControllerIndex[toUserId] = sc
		s.Unlock()
		go sc.runListener()
	} else {
		s.logger.Error().Str("toUser", toUserId).Msg("[slice] can't add sender: wrong number of encoding parameters")
	}
}

func (l *mixerSlice) scanInput(buf []byte, n int) {
	packet := &rtp.Packet{}
	packet.Unmarshal(buf)

	l.Lock()
	// estimation (x8 for bytes) not taking int account headers
	// it seems using MarshalSize (like for outputBits below) does not give the right numbers due to packet 0-padding
	l.inputBits += int64(n) * 8
	l.Unlock()
}

func (s *mixerSlice) Write(buf []byte) (err error) {
	packet := &rtp.Packet{}
	packet.Unmarshal(buf)
	err = s.output.WriteRTP(packet)

	if err == nil {
		go func() {
			outputBits := (packet.MarshalSize() - packet.Header.MarshalSize()) * 8
			s.Lock()
			s.outputBits += int64(outputBits)
			s.Unlock()
		}()
	}

	return
}

func (s *mixerSlice) loop() {
	pipeline, room, pc, userId := s.fromPs.pipeline, s.fromPs.r, s.fromPs.pc, s.fromPs.userId

	// returns a callback to push buffer to
	outputFiles := pipeline.BindTrack(s.kind, s)
	if s.kind == "video" {
		pipeline.BindPLICallback(func() {
			pc.throttledPLIRequest()
		})
	}
	if outputFiles != nil {
		room.addFiles(userId, outputFiles)
	}
	go s.runTickers()
	// go s.runReceiverListener()

	defer func() {
		s.logger.Info().Msgf("[slice] stopping %s track %s", s.kind, s.ID())
		s.stop()
	}()

	buf := make([]byte, defaultMTU)
	for {
		select {
		case <-room.endCh:
			// trial is over, no need to trigger signaling on every closing track
			return
		case <-s.fromPs.closedCh:
			// peer may quit early (for instance page refresh), other peers need to be updated
			return
		default:
			i, _, err := s.input.Read(buf)
			if err != nil {
				return
			}
			s.pipeline.PushRTP(s.kind, buf[:i])
			// for stats
			go s.scanInput(buf, i)
		}
	}
}

func (s *mixerSlice) runTickers() {
	roomId, userId := s.fromPs.r.id, s.fromPs.userId

	// update encoding bitrate on tick and according to minimum controller rate
	go func() {
		for range s.encoderTicker.C {
			if len(s.senderControllerIndex) > 0 {
				rates := []uint64{}
				for _, sc := range s.senderControllerIndex {
					rates = append(rates, sc.optimalBitrate)
				}
				sliceRate := minUint64Slice(rates)
				if s.pipeline != nil && sliceRate > 0 {
					s.Lock()
					s.optimalBitrate = sliceRate
					s.Unlock()
					s.pipeline.SetEncodingRate(s.kind, sliceRate)
				}
			}
		}
	}()

	go func() {
		for tickTime := range s.statsTicker.C {
			s.Lock()
			elapsed := tickTime.Sub(s.lastStats).Seconds()
			// update bitrates
			s.inputBitrate = s.inputBits / int64(elapsed)
			s.outputBitrate = s.outputBits / int64(elapsed)
			// reset cumulative bits and lastStats
			s.inputBits = 0
			s.outputBits = 0
			s.lastStats = tickTime
			s.Unlock()
			// log
			displayInputBitrateKbs := s.inputBitrate / 1000
			displayOutputBitrateKbs := s.outputBitrate / 1000
			log.Printf("[info] [room#%s] [user#%s] [mixer] %s input bitrate: %v kbit/s\n", roomId, userId, s.output.Kind().String(), displayInputBitrateKbs)
			log.Printf("[info] [room#%s] [user#%s] [mixer] %s output bitrate: %v kbit/s\n", roomId, userId, s.output.Kind().String(), displayOutputBitrateKbs)
		}
	}()

	// periodical log for video
	if s.output.Kind().String() == "video" {
		go func() {
			for range s.logTicker.C {
				display := fmt.Sprintf("%v kbit/s", s.optimalBitrate/1000)
				log.Printf("[info] [room#%s] [user#%s] [mixer] new target bitrate: %s\n", roomId, userId, display)
			}
		}()
	}
}

// func (s *mixerSlice) runReceiverListener() {
// 	roomId, userId := s.fromPs.r.id, s.fromPs.userId
// 	buf := make([]byte, defaultMTU)

// 	for {
// 		select {
// 		case <-s.endCh:
// 			return
// 		default:
// 			i, _, err := s.receiver.Read(buf)
// 			if err != nil {
// 				if err != io.EOF && err != io.ErrClosedPipe {
// 					log.Printf("[info] [room#%s] [user#%s] receiver read RTCP: %v\n", roomId, userId, err)
// 				}
// 				return
// 			}
// 			// TODO: send to rtpjitterbugger sink_rtcp
// 			//s.pipeline.PushRTCP(s.kind, buf[:i])

// 			// packets, err := rtcp.Unmarshal(buf[:i])
// 			// if err != nil {
// 			// 	log.Printf("[info] [room#%s] [user#%s] receiver unmarshal RTCP: %v\n", roomId, userId, err)
// 			// 	continue
// 			// }

// 			// for _, packet := range packets {
// 			// 	switch rtcpPacket := packet.(type) {
// 			// 	case *rtcp.SenderReport:
// 			// 		log.Println(rtcpPacket)
// 			// 	case *rtcp.ReceiverEstimatedMaximumBitrate:
// 			// 		log.Println(rtcpPacket)
// 			// 	default:
// 			// 		log.Printf("-- RTCP packet on receiver: %T", rtcpPacket)
// 			// 	}
// 			// }
// 		}
// 	}
// }

func (s *mixerSlice) controlFx(payload controlPayload) {
	interpolatorId := payload.Kind + payload.Name + payload.Property
	interpolator := s.interpolatorIndex[interpolatorId]

	if interpolator != nil {
		// an interpolation is already running for this pipeline, effect and property
		interpolator.Stop()
	}

	duration := payload.Duration
	if duration == 0 {
		s.pipeline.SetFxProp(s.kind, payload.Name, payload.Property, payload.Value)
	} else {
		if duration > maxInterpolatorDuration {
			duration = maxInterpolatorDuration
		}
		oldValue := s.pipeline.GetFxProp(s.kind, payload.Name, payload.Property)
		newInterpolator := sequencing.NewLinearInterpolator(oldValue, payload.Value, duration, defaultInterpolatorStep)

		s.Lock()
		s.interpolatorIndex[interpolatorId] = newInterpolator
		s.Unlock()

		defer func() {
			s.Lock()
			delete(s.interpolatorIndex, interpolatorId)
			s.Unlock()
		}()

		for {
			select {
			case <-s.fromPs.r.endCh:
				return
			case <-s.fromPs.closedCh:
				return
			case currentValue, more := <-newInterpolator.C:
				if more {
					s.pipeline.SetFxProp(s.kind, payload.Name, payload.Property, currentValue)
				} else {
					return
				}
			}
		}
	}
}
