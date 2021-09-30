// Package gst provides an easy API to create an appsink pipeline
package gst

/*
#cgo pkg-config: gstreamer-1.0 gstreamer-app-1.0
#include "gst.h"
*/
import "C"
import (
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/pion/webrtc/v3"
)

// global state
var (
	mu                sync.Mutex
	pipelineIndex     map[string]*Pipeline
	nvidia            bool
	forceEncodingSize bool
)

func init() {
	mu = sync.Mutex{}
	pipelineIndex = make(map[string]*Pipeline)
	nvidia = strings.ToLower(os.Getenv("DS_NVIDIA")) == "true"
	forceEncodingSize = strings.ToLower(os.Getenv("DS_FORCE_ENCODING_SIZE")) == "true"
}

// Pipeline is a wrapper for a GStreamer pipeline and output track
type Pipeline struct {
	// public
	Files []string
	// private
	id          string // same as local/output track id
	roomId      string
	userId      string
	gstPipeline *C.GstElement
	track       *webrtc.TrackLocalStaticRTP
	namespace   string
	filePrefix  string
	codec       string
	gpu         bool
}

func newPipelineStr(namespace string, filePrefix string, kind string, codec string, width int, height int, frameRate int, fx string, gpu bool) (pipelineStr string) {
	// special case for testing
	if fx == "passthrough" {
		pipelineStr = passthroughPipeline
		return
	}

	hasFx := len(fx) > 0
	var engine EngineSettings

	switch codec {
	case "opus":
		engine = settings.Opus
		if hasFx {
			pipelineStr = opusFxPipeline
		} else {
			pipelineStr = opusRawPipeline
		}
	case "VP8":
		engine = settings.VP8
		if hasFx {
			pipelineStr = vp8FxPipeline
		} else {
			pipelineStr = vp8RawPipeline
		}
	case "H264":
		if nvidia && gpu {
			engine = settings.NV264
		} else {
			engine = settings.X264
		}
		if hasFx {
			pipelineStr = h264FxPipeline
		} else {
			pipelineStr = h264RawPipeline
		}
	default:
		panic("Unhandled codec " + codec)
	}
	// set encoding and decoding
	pipelineStr = strings.Replace(pipelineStr, "${jitterBufferLatency}", settings.Common.JitterBufferLatency, -1)
	pipelineStr = strings.Replace(pipelineStr, "${encodeFast}", engine.Encode.Fast, -1)
	pipelineStr = strings.Replace(pipelineStr, "${encode}", engine.Encode.Relaxed, -1)
	pipelineStr = strings.Replace(pipelineStr, "${decode}", engine.Decode, -1)
	// set file
	pipelineStr = strings.Replace(pipelineStr, "${namespace}", namespace, -1)
	pipelineStr = strings.Replace(pipelineStr, "${prefix}", filePrefix, -1)
	// set fx
	if hasFx {
		// add "fx" prefix to avoid name clashes (for instance if a user gives the name "src")
		prefixedFx := strings.Replace(fx, "name=", "name=fx", 1)
		pipelineStr = strings.Replace(pipelineStr, "${fx}", prefixedFx, -1)
	}
	// set caps
	if forceEncodingSize {
		pipelineStr = strings.Replace(pipelineStr, "${widthCap}", ", width="+strconv.Itoa(width), -1)
		pipelineStr = strings.Replace(pipelineStr, "${heightCap}", ", height="+strconv.Itoa(height), -1)
		pipelineStr = strings.Replace(pipelineStr, "${framerateCap}", ", framerate="+strconv.Itoa(frameRate)+"/1", -1)
	} else {
		pipelineStr = strings.Replace(pipelineStr, "${widthCap}", "", -1)
		pipelineStr = strings.Replace(pipelineStr, "${heightCap}", "", -1)
		pipelineStr = strings.Replace(pipelineStr, "${framerateCap}", "", -1)
	}
	return
}

func fileName(namespace string, prefix string, kind string, suffix string) string {
	ext := ".mkv"
	if kind == "audio" {
		ext = ".ogg"
	}
	return namespace + "/" + prefix + "-" + kind + "-" + suffix + ext
}

func allFiles(namespace string, prefix string, kind string, hasFx bool) []string {
	if hasFx {
		return []string{fileName(namespace, prefix, kind, "in"), fileName(namespace, prefix, kind, "fx")}
	} else {
		return []string{fileName(namespace, prefix, kind, "in")}
	}
}

//export goStopCallback
func goStopCallback(cId *C.char) {
	mu.Lock()
	defer mu.Unlock()

	id := C.GoString(cId)
	pipeline, ok := pipelineIndex[id]
	if ok {
		log.Printf("[info] [room#%s] [user#%s] [output_track#%s] [pipeline] stop done\n", pipeline.roomId, pipeline.userId, id)

	}

	delete(pipelineIndex, id)

}

//export goNewSampleCallback
func goNewSampleCallback(cId *C.char, buffer unsafe.Pointer, bufferLen C.int, duration C.int) {
	id := C.GoString(cId)

	mu.Lock()
	pipeline, ok := pipelineIndex[id]
	mu.Unlock()

	if ok {
		if _, err := pipeline.track.Write(C.GoBytes(buffer, bufferLen)); err != nil {
			// TODO err contains the ID of the failing PeerConnections
			// we may store a callback on the Pipeline struct (the callback would remove those peers and update signaling)
			log.Printf("[error] [room#%s] [user#%s] [output_track#%s] [pipeline]  can't Write: %v\n", pipeline.roomId, pipeline.userId, id, err)
		}
	} else {
		// TODO return error to gst.c and stop processing?
		log.Printf("[error] [room#%s] [user#%s] [output_track#%s] [pipeline] pipeline not found, discarding buffer\n", pipeline.roomId, pipeline.userId, id)
	}
	C.free(buffer)
}

// API

func StartMainLoop() {
	C.gstreamer_start_mainloop()
}

// create a GStreamer pipeline
func CreatePipeline(roomId string, userId string, track *webrtc.TrackLocalStaticRTP, namespace string, filePrefix string, kind string, codec string, width int, height int, frameRate int, fx string, gpu bool) *Pipeline {

	pipelineStr := newPipelineStr(namespace, filePrefix, kind, codec, width, height, frameRate, fx, gpu)
	id := track.ID()
	log.Printf("[info] [room#%s] [user#%s] [output_track#%s] [pipeline]  %v pipeline initialized\n", roomId, userId, id, kind)
	log.Println(pipelineStr)

	cPipelineStr := C.CString(pipelineStr)
	cId := C.CString(id)
	defer C.free(unsafe.Pointer(cPipelineStr))
	defer C.free(unsafe.Pointer(cId))

	pipeline := &Pipeline{
		Files:       allFiles(namespace, filePrefix, kind, len(fx) > 0),
		id:          id,
		roomId:      roomId,
		userId:      userId,
		gstPipeline: C.gstreamer_parse_pipeline(cPipelineStr, cId),
		track:       track,
		namespace:   namespace,
		filePrefix:  filePrefix,
		codec:       codec,
		gpu:         gpu,
	}

	mu.Lock()
	pipelineIndex[pipeline.id] = pipeline
	mu.Unlock()
	return pipeline
}

// start the GStreamer pipeline
func (p *Pipeline) Start() {
	C.gstreamer_start_pipeline(p.gstPipeline)
	log.Printf("[info] [room#%s] [user#%s] [output_track#%s] [pipeline] started with recording prefix: %s/%s\n", p.roomId, p.userId, p.id, p.namespace, p.filePrefix)
}

// stop the GStreamer pipeline
func (p *Pipeline) Stop() {
	C.gstreamer_stop_pipeline(p.gstPipeline)
	log.Printf("[info] [room#%s] [user#%s] [output_track#%s] [pipeline] stop requested\n", p.roomId, p.userId, p.id)
}

// push a buffer on the appsrc of the GStreamer Pipeline
func (p *Pipeline) Push(buffer []byte) {
	b := C.CBytes(buffer)
	defer C.free(b)
	C.gstreamer_push_buffer(p.gstPipeline, b, C.int(len(buffer)))
}

func (p *Pipeline) getPropertyInt(name string, prop string) int {
	cName := C.CString(name)
	cProp := C.CString(prop)

	defer C.free(unsafe.Pointer(cName))
	defer C.free(unsafe.Pointer(cProp))

	return int(C.gstreamer_get_property_int(p.gstPipeline, cName, cProp))
}

func (p *Pipeline) setPropertyInt(name string, prop string, value int) {
	// fx prefix needed (added during pipeline initialization)
	cName := C.CString(name)
	cProp := C.CString(prop)
	cValue := C.int(value)

	defer C.free(unsafe.Pointer(cName))
	defer C.free(unsafe.Pointer(cProp))

	C.gstreamer_set_property_int(p.gstPipeline, cName, cProp, cValue)
}

func (p *Pipeline) setPropertyFloat(name string, prop string, value float32) {
	// fx prefix needed (added during pipeline initialization)
	cName := C.CString(name)
	cProp := C.CString(prop)
	cValue := C.float(value)

	defer C.free(unsafe.Pointer(cName))
	defer C.free(unsafe.Pointer(cProp))

	C.gstreamer_set_property_float(p.gstPipeline, cName, cProp, cValue)
}

func (p *Pipeline) SetEncodingRate(value64 uint64) {
	value := int(value64)
	// see https://gstreamer.freedesktop.org/documentation/x264/index.html?gi-language=c#x264enc:bitrate
	// see https://gstreamer.freedesktop.org/documentation/nvcodec/GstNvBaseEnc.html?gi-language=c#GstNvBaseEnc:bitrate
	// see https://gstreamer.freedesktop.org/documentation/opus/opusenc.html?gi-language=c#opusenc:bitrate
	prop := "bitrate"
	if p.codec == "VP8" {
		// see https://gstreamer.freedesktop.org/documentation/vpx/GstVPXEnc.html?gi-language=c#GstVPXEnc:target-bitrate
		prop = "target-bitrate"
	} else if p.codec == "H264" {
		// in kbit/s for x264enc and nvh264enc
		value = value / 1000
		if p.gpu {
			// acts both on bitrate and max-bitrate for nvh264enc
			p.setPropertyInt("encoder", "max-bitrate", value*320/256)
		}
	}
	p.setPropertyInt("encoder", prop, value)
}

func (p *Pipeline) SetFxProperty(name string, prop string, value float32) {
	// fx prefix needed (added during pipeline initialization)
	p.setPropertyFloat("fx"+name, prop, value)
}

func (p *Pipeline) GetFxProperty(name string, prop string) float32 {
	// fx prefix needed (added during pipeline initialization)
	cName := C.CString("fx" + name)
	cProp := C.CString(prop)

	defer C.free(unsafe.Pointer(cName))
	defer C.free(unsafe.Pointer(cProp))

	return float32(C.gstreamer_get_property_float(p.gstPipeline, cName, cProp))
}
