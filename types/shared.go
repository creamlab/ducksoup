package types

type JoinPayload struct {
	RoomId   string `json:"roomId"`
	UserId   string `json:"userId"`
	Duration int    `json:"duration"`
	// optional
	Namespace     string `json:"namespace"`
	VideoFormat   string `json:"videoFormat"`
	RecordingMode string `json:"recordingMode"`
	Size          int    `json:"size"`
	AudioFx       string `json:"audioFx"`
	VideoFx       string `json:"videoFx"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	FrameRate     int    `json:"frameRate"`
	GPU           bool   `json:"gpu"`
	// Not from JSON
	Origin string
}

type TrackWriter interface {
	ID() string
	Write(buf []byte) error
}
