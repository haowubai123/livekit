package sfu

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	dd "github.com/livekit/livekit-server/pkg/sfu/dependencydescriptor"
)

// Forwarder
const (
	FlagPauseOnDowngrade     = true
	FlagFilterRTX            = true
	TransitionCostSpatial    = 10
	ParkedLayersWaitDuration = 2 * time.Second
)

// -------------------------------------------------------------------

type VideoPauseReason int

const (
	VideoPauseReasonNone VideoPauseReason = iota
	VideoPauseReasonMuted
	VideoPauseReasonPubMuted
	VideoPauseReasonFeedDry
	VideoPauseReasonBandwidth
)

func (v VideoPauseReason) String() string {
	switch v {
	case VideoPauseReasonNone:
		return "NONE"
	case VideoPauseReasonMuted:
		return "MUTED"
	case VideoPauseReasonPubMuted:
		return "PUB_MUTED"
	case VideoPauseReasonFeedDry:
		return "FEED_DRY"
	case VideoPauseReasonBandwidth:
		return "BANDWIDTH"
	default:
		return fmt.Sprintf("%d", int(v))
	}
}

// -------------------------------------------------------------------

type VideoAllocation struct {
	PauseReason         VideoPauseReason
	IsDeficient         bool
	BandwidthRequested  int64
	BandwidthDelta      int64
	BandwidthNeeded     int64
	Bitrates            Bitrates
	TargetLayers        buffer.VideoLayer
	RequestLayerSpatial int32
	MaxLayers           buffer.VideoLayer
	DistanceToDesired   float64
}

func (v VideoAllocation) String() string {
	return fmt.Sprintf("VideoAllocation{pause: %s, def: %+v, bwr: %d, del: %d, bwn: %d, rates: %+v, target: %s, req: %d, max: %s, dist: %0.2f}",
		v.PauseReason,
		v.IsDeficient,
		v.BandwidthRequested,
		v.BandwidthDelta,
		v.BandwidthNeeded,
		v.Bitrates,
		v.TargetLayers,
		v.RequestLayerSpatial,
		v.MaxLayers,
		v.DistanceToDesired,
	)
}

var (
	VideoAllocationDefault = VideoAllocation{
		PauseReason:         VideoPauseReasonFeedDry, // start with no feed till feed is seen
		TargetLayers:        buffer.InvalidLayers,
		RequestLayerSpatial: buffer.InvalidLayerSpatial,
		MaxLayers:           buffer.InvalidLayers,
	}
)

// -------------------------------------------------------------------

type VideoAllocationProvisional struct {
	muted                bool
	pubMuted             bool
	maxPublishedLayer    int32
	maxTemporalLayerSeen int32
	availableLayers      []int32
	Bitrates             Bitrates
	maxLayers            buffer.VideoLayer
	currentLayers        buffer.VideoLayer
	parkedLayers         buffer.VideoLayer
	allocatedLayers      buffer.VideoLayer
}

// -------------------------------------------------------------------

type VideoTransition struct {
	From           buffer.VideoLayer
	To             buffer.VideoLayer
	BandwidthDelta int64
}

func (v VideoTransition) String() string {
	return fmt.Sprintf("VideoTransition{from: %s, to: %s, del: %d}", v.From, v.To, v.BandwidthDelta)
}

// -------------------------------------------------------------------

type TranslationParams struct {
	shouldDrop            bool
	isDroppingRelevant    bool
	isSwitchingToMaxLayer bool
	rtp                   *TranslationParamsRTP
	vp8                   *TranslationParamsVP8
	ddExtension           *dd.DependencyDescriptorExtension
	marker                bool

	// indicates this frame has 'Switch' decode indication for target layer
	// TODO : in theory, we need check frame chain is not broken for the target
	// but we don't have frame queue now, so just use decode target indication
	isSwitchingToTargetLayer bool
}

// -------------------------------------------------------------------

type ForwarderState struct {
	Started bool
	RTP     RTPMungerState
	VP8     VP8MungerState
}

func (f ForwarderState) String() string {
	return fmt.Sprintf("ForwarderState{started: %v, rtp: %s, vp8: %s}", f.Started, f.RTP.String(), f.VP8.String())
}

// -------------------------------------------------------------------

type Forwarder struct {
	lock                          sync.RWMutex
	codec                         webrtc.RTPCodecCapability
	kind                          webrtc.RTPCodecType
	logger                        logger.Logger
	getReferenceLayerRTPTimestamp func(ts uint32, layer int32, referenceLayer int32) (uint32, error)

	muted    bool
	pubMuted bool

	maxPublishedLayer    int32
	maxTemporalLayerSeen int32

	started               bool
	lastSSRC              uint32
	referenceLayerSpatial int32

	maxLayers           buffer.VideoLayer
	currentLayers       buffer.VideoLayer
	targetLayers        buffer.VideoLayer
	requestLayerSpatial int32
	parkedLayers        buffer.VideoLayer // layers that can resume without key frame
	parkedLayersTimer   *time.Timer

	provisional *VideoAllocationProvisional

	lastAllocation VideoAllocation

	rtpMunger *RTPMunger
	vp8Munger *VP8Munger

	isTemporalSupported bool

	ddLayerSelector *DDVideoLayerSelector

	onParkedLayersExpired func()
}

func NewForwarder(
	kind webrtc.RTPCodecType,
	logger logger.Logger,
	getReferenceLayerRTPTimestamp func(ts uint32, layer int32, referenceLayer int32) (uint32, error),
) *Forwarder {
	f := &Forwarder{
		kind:                          kind,
		logger:                        logger,
		getReferenceLayerRTPTimestamp: getReferenceLayerRTPTimestamp,

		maxPublishedLayer:    buffer.InvalidLayerSpatial,
		maxTemporalLayerSeen: buffer.InvalidLayerTemporal,

		referenceLayerSpatial: buffer.InvalidLayerSpatial,

		// start off with nothing, let streamallocator/opportunistic forwarder set the target
		currentLayers:       buffer.InvalidLayers,
		targetLayers:        buffer.InvalidLayers,
		requestLayerSpatial: buffer.InvalidLayerSpatial,
		parkedLayers:        buffer.InvalidLayers,

		lastAllocation: VideoAllocationDefault,

		rtpMunger: NewRTPMunger(logger),
	}

	if f.kind == webrtc.RTPCodecTypeVideo {
		f.maxLayers = buffer.VideoLayer{Spatial: buffer.InvalidLayerSpatial, Temporal: buffer.DefaultMaxLayerTemporal}
	} else {
		f.maxLayers = buffer.InvalidLayers
	}

	return f
}

func (f *Forwarder) SetMaxPublishedLayer(maxPublishedLayer int32) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if maxPublishedLayer <= f.maxPublishedLayer {
		return
	}

	f.maxPublishedLayer = maxPublishedLayer
	f.logger.Debugw("setting max published layer", "maxPublishedLayer", f.maxPublishedLayer)
}

func (f *Forwarder) SetMaxTemporalLayerSeen(maxTemporalLayerSeen int32) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if maxTemporalLayerSeen <= f.maxTemporalLayerSeen {
		return
	}

	f.maxTemporalLayerSeen = maxTemporalLayerSeen
	f.logger.Debugw("setting max temporal layer seen", "maxTemporalLayerSeen", f.maxTemporalLayerSeen)
}

func (f *Forwarder) OnParkedLayersExpired(fn func()) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.onParkedLayersExpired = fn
}

func (f *Forwarder) getOnParkedLayersExpired() func() {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.onParkedLayersExpired
}

func (f *Forwarder) DetermineCodec(codec webrtc.RTPCodecCapability) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.codec.MimeType != "" {
		return
	}
	f.codec = codec

	switch strings.ToLower(codec.MimeType) {
	case "video/vp8":
		f.isTemporalSupported = true
		f.vp8Munger = NewVP8Munger(f.logger)
	case "video/av1", "video/vp9":
		// TODO : we only enable dd layer selector for av1 and vp9 now, at future we can
		// enable it for vp8 too
		f.ddLayerSelector = NewDDVideoLayerSelector(f.logger)
	}
}

func (f *Forwarder) GetState() ForwarderState {
	f.lock.RLock()
	defer f.lock.RUnlock()

	if !f.started {
		return ForwarderState{}
	}

	state := ForwarderState{
		Started: f.started,
		RTP:     f.rtpMunger.GetLast(),
	}

	if f.vp8Munger != nil {
		state.VP8 = f.vp8Munger.GetLast()
	}

	return state
}

func (f *Forwarder) SeedState(state ForwarderState) {
	if !state.Started {
		return
	}

	f.lock.Lock()
	defer f.lock.Unlock()

	f.rtpMunger.SeedLast(state.RTP)
	if f.vp8Munger != nil {
		f.vp8Munger.SeedLast(state.VP8)
	}

	f.started = true
}

func (f *Forwarder) Mute(muted bool) (bool, buffer.VideoLayer) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.muted == muted {
		return false, f.maxLayers
	}

	f.logger.Debugw("setting forwarder mute", "muted", muted)
	f.muted = muted

	// resync when muted so that sequence numbers do not jump on unmute
	if muted {
		f.resyncLocked()
	}

	return true, f.maxLayers
}

func (f *Forwarder) IsMuted() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.muted
}

func (f *Forwarder) PubMute(pubMuted bool) (bool, buffer.VideoLayer) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.pubMuted == pubMuted {
		return false, f.maxLayers
	}

	f.logger.Debugw("setting forwarder pub mute", "pubMuted", pubMuted)
	f.pubMuted = pubMuted

	if f.kind == webrtc.RTPCodecTypeAudio {
		// for audio resync when pub muted so that sequence numbers do not jump on unmute
		// audio stops forwarding during pub mute too
		if pubMuted {
			f.resyncLocked()
		}
	} else {
		// Do not resync on publisher mute as forwarding can continue on unmute using same layers.
		// On unmute, park current layers as streaming can continue without a key frame when publisher starts the stream.
		if !pubMuted && f.targetLayers.IsValid() && f.currentLayers.Spatial == f.targetLayers.Spatial {
			f.setupParkedLayers(f.targetLayers)
			f.currentLayers = buffer.InvalidLayers
		}
	}

	return true, f.maxLayers
}

func (f *Forwarder) IsPubMuted() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.pubMuted
}

func (f *Forwarder) IsAnyMuted() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.muted || f.pubMuted
}

func (f *Forwarder) SetMaxSpatialLayer(spatialLayer int32) (bool, buffer.VideoLayer, buffer.VideoLayer) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio || spatialLayer == f.maxLayers.Spatial {
		return false, f.maxLayers, f.currentLayers
	}

	f.logger.Debugw("setting max spatial layer", "layer", spatialLayer)
	f.maxLayers.Spatial = spatialLayer

	f.clearParkedLayers()

	return true, f.maxLayers, f.currentLayers
}

func (f *Forwarder) SetMaxTemporalLayer(temporalLayer int32) (bool, buffer.VideoLayer, buffer.VideoLayer) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio || temporalLayer == f.maxLayers.Temporal {
		return false, f.maxLayers, f.currentLayers
	}

	f.logger.Debugw("setting max temporal layer", "layer", temporalLayer)
	f.maxLayers.Temporal = temporalLayer

	f.clearParkedLayers()

	return true, f.maxLayers, f.currentLayers
}

func (f *Forwarder) MaxLayers() buffer.VideoLayer {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.maxLayers
}

func (f *Forwarder) CurrentLayers() buffer.VideoLayer {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.currentLayers
}

func (f *Forwarder) TargetLayers() buffer.VideoLayer {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.targetLayers
}

func (f *Forwarder) GetReferenceLayerSpatial() int32 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.referenceLayerSpatial
}

func (f *Forwarder) isDeficientLocked() bool {
	return f.lastAllocation.IsDeficient
}

func (f *Forwarder) IsDeficient() bool {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.isDeficientLocked()
}

func (f *Forwarder) BandwidthRequested() int64 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.lastAllocation.BandwidthRequested
}

func (f *Forwarder) DistanceToDesired(availableLayers []int32, brs Bitrates) float64 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return getDistanceToDesired(
		f.muted,
		f.pubMuted,
		f.maxPublishedLayer,
		f.maxTemporalLayerSeen,
		availableLayers,
		brs,
		f.targetLayers,
		f.maxLayers,
	)
}

func (f *Forwarder) GetOptimalBandwidthNeeded(brs Bitrates) int64 {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return getOptimalBandwidthNeeded(f.muted, f.pubMuted, f.maxPublishedLayer, brs, f.maxLayers)
}

func (f *Forwarder) AllocateOptimal(availableLayers []int32, brs Bitrates, allowOvershoot bool) VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return f.lastAllocation
	}

	alloc := VideoAllocation{
		PauseReason:         VideoPauseReasonNone,
		Bitrates:            brs,
		TargetLayers:        buffer.InvalidLayers,
		RequestLayerSpatial: f.requestLayerSpatial,
		MaxLayers:           f.maxLayers,
	}
	optimalBandwidthNeeded := getOptimalBandwidthNeeded(f.muted, f.pubMuted, f.maxPublishedLayer, brs, f.maxLayers)
	if optimalBandwidthNeeded == 0 {
		alloc.PauseReason = VideoPauseReasonFeedDry
	}
	alloc.BandwidthNeeded = optimalBandwidthNeeded

	opportunisticAlloc := func() {
		// opportunistically latch on to anything
		maxSpatial := f.maxLayers.Spatial
		if allowOvershoot && f.maxPublishedLayer > maxSpatial {
			maxSpatial = f.maxPublishedLayer
		}
		alloc.TargetLayers = buffer.VideoLayer{
			Spatial:  int32(math.Min(float64(f.maxPublishedLayer), float64(maxSpatial))),
			Temporal: f.maxLayers.Temporal,
		}
	}

	switch {
	case !f.maxLayers.IsValid() || f.maxPublishedLayer == buffer.InvalidLayerSpatial:
		// nothing to do when max layers are not valid OR max publisher layer is invalid

	case f.muted:
		alloc.PauseReason = VideoPauseReasonMuted

	case f.pubMuted:
		alloc.PauseReason = VideoPauseReasonPubMuted
		// leave it at current layers for opportunistic resume
		alloc.TargetLayers = f.currentLayers
		alloc.RequestLayerSpatial = alloc.TargetLayers.Spatial

	case f.parkedLayers.IsValid():
		// if parked on a layer, let it continue
		alloc.TargetLayers = f.parkedLayers
		alloc.RequestLayerSpatial = alloc.TargetLayers.Spatial

	case len(availableLayers) == 0:
		// feed may be dry
		if f.currentLayers.IsValid() {
			// let it continue at current layer if valid.
			// Covers the cases of
			//   1. mis-detection of layer stop - can continue streaming
			//   2. current layer resuming - can latch on when it starts
			alloc.TargetLayers = f.currentLayers
			alloc.RequestLayerSpatial = alloc.TargetLayers.Spatial
		} else {
			// opportunistically latch on to anything
			opportunisticAlloc()
			alloc.RequestLayerSpatial = int32(math.Min(float64(f.maxLayers.Spatial), float64(f.maxPublishedLayer)))
		}

	default:
		isCurrentLayerAvailable := false
		if f.currentLayers.IsValid() {
			for _, l := range availableLayers {
				if l == f.currentLayers.Spatial {
					isCurrentLayerAvailable = true
					break
				}
			}
		}

		if !isCurrentLayerAvailable && f.currentLayers.IsValid() {
			// current layer maybe stopped, move to highest available
			for _, l := range availableLayers {
				if l > alloc.TargetLayers.Spatial {
					alloc.TargetLayers.Spatial = l
				}
			}
			alloc.TargetLayers.Temporal = f.maxLayers.Temporal

			alloc.RequestLayerSpatial = alloc.TargetLayers.Spatial
		} else {
			requestLayerSpatial := int32(math.Min(float64(f.maxLayers.Spatial), float64(f.maxPublishedLayer)))
			if f.currentLayers.IsValid() && requestLayerSpatial == f.requestLayerSpatial && f.currentLayers.Spatial == f.requestLayerSpatial {
				// current is locked to desired, stay there
				alloc.TargetLayers = buffer.VideoLayer{Spatial: f.requestLayerSpatial, Temporal: f.maxLayers.Temporal}
				alloc.RequestLayerSpatial = f.requestLayerSpatial
			} else {
				// opportunistically latch on to anything
				opportunisticAlloc()
				alloc.RequestLayerSpatial = requestLayerSpatial
			}
		}
	}

	if !alloc.TargetLayers.IsValid() {
		alloc.TargetLayers = buffer.InvalidLayers
		alloc.RequestLayerSpatial = buffer.InvalidLayerSpatial
	}
	if alloc.TargetLayers.IsValid() {
		alloc.BandwidthRequested = optimalBandwidthNeeded
	}
	alloc.BandwidthDelta = alloc.BandwidthRequested - f.lastAllocation.BandwidthRequested
	alloc.DistanceToDesired = getDistanceToDesired(
		f.muted,
		f.pubMuted,
		f.maxPublishedLayer,
		f.maxTemporalLayerSeen,
		availableLayers,
		brs,
		alloc.TargetLayers,
		f.maxLayers,
	)

	return f.updateAllocation(alloc, "optimal")
}

func (f *Forwarder) ProvisionalAllocatePrepare(availableLayers []int32, Bitrates Bitrates) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.provisional = &VideoAllocationProvisional{
		allocatedLayers:      buffer.InvalidLayers,
		muted:                f.muted,
		pubMuted:             f.pubMuted,
		maxPublishedLayer:    f.maxPublishedLayer,
		maxTemporalLayerSeen: f.maxTemporalLayerSeen,
		Bitrates:             Bitrates,
		maxLayers:            f.maxLayers,
		currentLayers:        f.currentLayers,
		parkedLayers:         f.parkedLayers,
	}

	f.provisional.availableLayers = make([]int32, len(availableLayers))
	copy(f.provisional.availableLayers, availableLayers)
}

func (f *Forwarder) ProvisionalAllocate(availableChannelCapacity int64, layers buffer.VideoLayer, allowPause bool, allowOvershoot bool) int64 {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.provisional.muted || f.provisional.pubMuted || f.provisional.maxPublishedLayer == buffer.InvalidLayerSpatial || !f.provisional.maxLayers.IsValid() || (!allowOvershoot && layers.GreaterThan(f.provisional.maxLayers)) {
		return 0
	}

	requiredBitrate := f.provisional.Bitrates[layers.Spatial][layers.Temporal]
	if requiredBitrate == 0 {
		return 0
	}

	alreadyAllocatedBitrate := int64(0)
	if f.provisional.allocatedLayers.IsValid() {
		alreadyAllocatedBitrate = f.provisional.Bitrates[f.provisional.allocatedLayers.Spatial][f.provisional.allocatedLayers.Temporal]
	}

	// a layer under maximum fits, take it
	if !layers.GreaterThan(f.provisional.maxLayers) && requiredBitrate <= (availableChannelCapacity+alreadyAllocatedBitrate) {
		f.provisional.allocatedLayers = layers
		return requiredBitrate - alreadyAllocatedBitrate
	}

	//
	// Given layer does not fit. But overshoot is allowed.
	// Could be one of
	//  1. a layer below maximum that does not fit
	//  2. a layer above maximum which may or may not fit.
	// In any of those cases, take the lowest possible layer if pause is not allowed
	//
	if !allowPause && (!f.provisional.allocatedLayers.IsValid() || !layers.GreaterThan(f.provisional.allocatedLayers)) {
		f.provisional.allocatedLayers = layers
		return requiredBitrate - alreadyAllocatedBitrate
	}

	return 0
}

func (f *Forwarder) ProvisionalAllocateGetCooperativeTransition(allowOvershoot bool) VideoTransition {
	//
	// This is called when a track needs a change (could be mute/unmute, subscribed layers changed, published layers changed)
	// when channel is congested.
	//
	// The goal is to provide a co-operative transition. Co-operative stream allocation aims to keep all the streams active
	// as much as possible.
	//
	// When channel is congested, effecting a transition which will consume more bits will lead to more congestion.
	// So, this routine does the following
	//   1. When muting, it is not going to increase consumption.
	//   2. If the stream is currently active and the transition needs more bits (higher layers = more bits), do not make the up move.
	//      The higher layer requirement could be due to a new published layer becoming available or subscribed layers changing.
	//   3. If the new target layers are lower than current target, take the move down and save bits.
	//   4. If not currently streaming, find the minimum layers that can unpause the stream.
	//
	// To summarize, co-operative streaming means
	//   - Try to keep tracks streaming, i.e. no pauses at the expense of some streams not being at optimal layers
	//   - Do not make an upgrade as it could affect other tracks
	//
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.provisional.muted || f.provisional.pubMuted {
		f.provisional.allocatedLayers = buffer.InvalidLayers
		if f.provisional.pubMuted {
			// leave it at current for opportunistic forwarding, there is still bandwidth saving with publisher mute
			f.provisional.allocatedLayers = f.provisional.currentLayers
		}
		return VideoTransition{
			From:           f.targetLayers,
			To:             f.provisional.allocatedLayers,
			BandwidthDelta: 0 - f.lastAllocation.BandwidthRequested,
		}
	}

	// check if we should preserve current target
	if f.targetLayers.IsValid() {
		// what is the highest that is available
		maximalLayers := buffer.InvalidLayers
		maximalBandwidthRequired := int64(0)
		for s := f.provisional.maxLayers.Spatial; s >= 0; s-- {
			for t := f.provisional.maxLayers.Temporal; t >= 0; t-- {
				if f.provisional.Bitrates[s][t] != 0 {
					maximalLayers = buffer.VideoLayer{Spatial: s, Temporal: t}
					maximalBandwidthRequired = f.provisional.Bitrates[s][t]
					break
				}
			}

			if maximalBandwidthRequired != 0 {
				break
			}
		}

		if maximalLayers.IsValid() {
			if !f.targetLayers.GreaterThan(maximalLayers) && f.provisional.Bitrates[f.targetLayers.Spatial][f.targetLayers.Temporal] != 0 {
				// currently streaming and maybe wanting an upgrade (f.targetLayers <= maximalLayers),
				// just preserve current target in the cooperative scheme of things
				f.provisional.allocatedLayers = f.targetLayers
				return VideoTransition{
					From:           f.targetLayers,
					To:             f.targetLayers,
					BandwidthDelta: 0,
				}
			}

			if f.targetLayers.GreaterThan(maximalLayers) {
				// maximalLayers < f.targetLayers, make the down move
				f.provisional.allocatedLayers = maximalLayers
				return VideoTransition{
					From:           f.targetLayers,
					To:             maximalLayers,
					BandwidthDelta: maximalBandwidthRequired - f.lastAllocation.BandwidthRequested,
				}
			}
		}
	}

	findNextLayer := func(
		minSpatial, maxSpatial int32,
		minTemporal, maxTemporal int32,
	) (buffer.VideoLayer, int64) {
		layers := buffer.InvalidLayers
		bw := int64(0)
		for s := minSpatial; s <= maxSpatial; s++ {
			for t := minTemporal; t <= maxTemporal; t++ {
				if f.provisional.Bitrates[s][t] != 0 {
					layers = buffer.VideoLayer{Spatial: s, Temporal: t}
					bw = f.provisional.Bitrates[s][t]
					break
				}
			}

			if bw != 0 {
				break
			}
		}

		return layers, bw
	}

	targetLayers := f.targetLayers
	bandwidthRequired := int64(0)
	if !targetLayers.IsValid() {
		// currently not streaming, find minimal
		// NOTE: a layer in feed could have paused and there could be other options than going back to minimal,
		// but the cooperative scheme knocks things back to minimal
		targetLayers, bandwidthRequired = findNextLayer(
			0, f.provisional.maxLayers.Spatial,
			0, f.provisional.maxLayers.Temporal,
		)

		// could not find a minimal layer, overshoot if allowed
		if bandwidthRequired == 0 && f.provisional.maxLayers.IsValid() && allowOvershoot {
			targetLayers, bandwidthRequired = findNextLayer(
				f.provisional.maxLayers.Spatial+1, buffer.DefaultMaxLayerSpatial,
				0, buffer.DefaultMaxLayerTemporal,
			)
		}
	}

	// if nothing available, just leave target at current to enable opportunistic forwarding in case current resumes
	if !targetLayers.IsValid() {
		if f.provisional.parkedLayers.IsValid() {
			targetLayers = f.provisional.parkedLayers
		} else {
			targetLayers = f.provisional.currentLayers
		}
	}

	f.provisional.allocatedLayers = targetLayers
	return VideoTransition{
		From:           f.targetLayers,
		To:             targetLayers,
		BandwidthDelta: bandwidthRequired - f.lastAllocation.BandwidthRequested,
	}
}

func (f *Forwarder) ProvisionalAllocateGetBestWeightedTransition() VideoTransition {
	//
	// This is called when a track needs a change (could be mute/unmute, subscribed layers changed, published layers changed)
	// when channel is congested. This is called on tracks other than the one needing the change. When the track
	// needing the change requires bits, this is called to check if this track can contribute some bits to the pool.
	//
	// The goal is to keep all tracks streaming as much as possible. So, the track that needs a change needs bandwidth to be unpaused.
	//
	// This tries to figure out how much this track can contribute back to the pool to enable the track that needs to be unpaused.
	//   1. Track muted OR feed dry - can contribute everything back in case it was using bandwidth.
	//   2. Look at all possible down transitions from current target and find the best offer.
	//      Best offer is calculated as bandwidth saved moving to a down layer divided by cost.
	//      Cost has two components
	//        a. Transition cost: Spatial layer switch is expensive due to key frame requirement, but temporal layer switch is free.
	//        b. Quality cost: The farther away from desired layers, the higher the quality cost.
	//
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.provisional.muted || f.provisional.pubMuted {
		f.provisional.allocatedLayers = buffer.InvalidLayers
		if f.provisional.pubMuted {
			// leave it at current for opportunistic forwarding, there is still bandwidth saving with publisher mute
			f.provisional.allocatedLayers = f.provisional.currentLayers
		}
		return VideoTransition{
			From:           f.targetLayers,
			To:             f.provisional.allocatedLayers,
			BandwidthDelta: 0 - f.lastAllocation.BandwidthRequested,
		}
	}

	maxReachableLayerTemporal := buffer.InvalidLayerTemporal
	for t := f.provisional.maxLayers.Temporal; t >= 0; t-- {
		for s := f.provisional.maxLayers.Spatial; s >= 0; s-- {
			if f.provisional.Bitrates[s][t] != 0 {
				maxReachableLayerTemporal = t
				break
			}
		}
		if maxReachableLayerTemporal != buffer.InvalidLayerTemporal {
			break
		}
	}

	if maxReachableLayerTemporal == buffer.InvalidLayerTemporal {
		// feed has gone dry, just leave target at current to enable opportunistic forwarding in case current resumes.
		// Note that this is giving back bits and opportunistic forwarding resuming might trigger congestion again,
		// but that should be handled by stream allocator.
		if f.provisional.parkedLayers.IsValid() {
			f.provisional.allocatedLayers = f.provisional.parkedLayers
		} else {
			f.provisional.allocatedLayers = f.provisional.currentLayers
		}
		return VideoTransition{
			From:           f.targetLayers,
			To:             f.provisional.allocatedLayers,
			BandwidthDelta: 0 - f.lastAllocation.BandwidthRequested,
		}
	}

	// starting from minimum to target, find transition which gives the best
	// transition taking into account bits saved vs cost of such a transition
	bestLayers := buffer.InvalidLayers
	bestBandwidthDelta := int64(0)
	bestValue := float32(0)
	for s := int32(0); s <= f.targetLayers.Spatial; s++ {
		for t := int32(0); t <= f.targetLayers.Temporal; t++ {
			if s == f.targetLayers.Spatial && t == f.targetLayers.Temporal {
				break
			}

			BandwidthDelta := int64(math.Max(float64(0), float64(f.lastAllocation.BandwidthRequested-f.provisional.Bitrates[s][t])))

			transitionCost := int32(0)
			if f.targetLayers.Spatial != s {
				transitionCost = TransitionCostSpatial
			}

			qualityCost := (maxReachableLayerTemporal+1)*(f.targetLayers.Spatial-s) + (f.targetLayers.Temporal - t)

			value := float32(0)
			if (transitionCost + qualityCost) != 0 {
				value = float32(BandwidthDelta) / float32(transitionCost+qualityCost)
			}
			if value > bestValue || (value == bestValue && BandwidthDelta > bestBandwidthDelta) {
				bestValue = value
				bestBandwidthDelta = BandwidthDelta
				bestLayers = buffer.VideoLayer{Spatial: s, Temporal: t}
			}
		}
	}

	f.provisional.allocatedLayers = bestLayers
	return VideoTransition{
		From:           f.targetLayers,
		To:             bestLayers,
		BandwidthDelta: bestBandwidthDelta,
	}
}

func (f *Forwarder) ProvisionalAllocateCommit() VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	optimalBandwidthNeeded := getOptimalBandwidthNeeded(
		f.provisional.muted,
		f.provisional.pubMuted,
		f.provisional.maxPublishedLayer,
		f.provisional.Bitrates,
		f.provisional.maxLayers,
	)
	alloc := VideoAllocation{
		BandwidthRequested:  0,
		BandwidthDelta:      -f.lastAllocation.BandwidthRequested,
		Bitrates:            f.provisional.Bitrates,
		BandwidthNeeded:     optimalBandwidthNeeded,
		TargetLayers:        f.provisional.allocatedLayers,
		RequestLayerSpatial: f.provisional.allocatedLayers.Spatial,
		MaxLayers:           f.provisional.maxLayers,
		DistanceToDesired: getDistanceToDesired(
			f.provisional.muted,
			f.provisional.pubMuted,
			f.provisional.maxPublishedLayer,
			f.provisional.maxTemporalLayerSeen,
			f.provisional.availableLayers,
			f.provisional.Bitrates,
			f.provisional.allocatedLayers,
			f.provisional.maxLayers,
		),
	}

	switch {
	case f.provisional.muted:
		alloc.PauseReason = VideoPauseReasonMuted

	case f.provisional.pubMuted:
		alloc.PauseReason = VideoPauseReasonPubMuted

	case optimalBandwidthNeeded == 0:
		if f.provisional.allocatedLayers.IsValid() {
			// overshoot
			alloc.BandwidthRequested = f.provisional.Bitrates[f.provisional.allocatedLayers.Spatial][f.provisional.allocatedLayers.Temporal]
			alloc.BandwidthDelta = alloc.BandwidthRequested - f.lastAllocation.BandwidthRequested
		} else {
			alloc.PauseReason = VideoPauseReasonFeedDry

			// leave target at current for opportunistic forwarding
			if f.provisional.currentLayers.IsValid() && f.provisional.currentLayers.Spatial <= f.provisional.maxLayers.Spatial {
				f.provisional.allocatedLayers = f.provisional.currentLayers
				alloc.TargetLayers = f.provisional.allocatedLayers
				alloc.RequestLayerSpatial = alloc.TargetLayers.Spatial
			}
		}

	default:
		if f.provisional.allocatedLayers.IsValid() {
			alloc.BandwidthRequested = f.provisional.Bitrates[f.provisional.allocatedLayers.Spatial][f.provisional.allocatedLayers.Temporal]
		}
		alloc.BandwidthDelta = alloc.BandwidthRequested - f.lastAllocation.BandwidthRequested

		if f.provisional.allocatedLayers.GreaterThan(f.provisional.maxLayers) ||
			alloc.BandwidthRequested >= getOptimalBandwidthNeeded(
				f.provisional.muted,
				f.provisional.pubMuted,
				f.provisional.maxPublishedLayer,
				f.provisional.Bitrates,
				f.provisional.maxLayers,
			) {
			// could be greater than optimal if overshooting
			alloc.IsDeficient = false
		} else {
			alloc.IsDeficient = true
			if !f.provisional.allocatedLayers.IsValid() {
				alloc.PauseReason = VideoPauseReasonBandwidth
			}
		}
	}

	f.clearParkedLayers()
	return f.updateAllocation(alloc, "cooperative")
}

func (f *Forwarder) AllocateNextHigher(availableChannelCapacity int64, availableLayers []int32, brs Bitrates, allowOvershoot bool) (VideoAllocation, bool) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return f.lastAllocation, false
	}

	// if not deficient, nothing to do
	if !f.isDeficientLocked() {
		return f.lastAllocation, false
	}

	// if targets are still pending, don't increase
	if f.targetLayers.IsValid() && f.targetLayers != f.currentLayers {
		return f.lastAllocation, false
	}

	optimalBandwidthNeeded := getOptimalBandwidthNeeded(f.muted, f.pubMuted, f.maxPublishedLayer, brs, f.maxLayers)

	alreadyAllocated := int64(0)
	if f.targetLayers.IsValid() {
		alreadyAllocated = brs[f.targetLayers.Spatial][f.targetLayers.Temporal]
	}

	doAllocation := func(
		minSpatial, maxSpatial int32,
		minTemporal, maxTemporal int32,
	) (bool, VideoAllocation, bool) {
		for s := minSpatial; s <= maxSpatial; s++ {
			for t := minTemporal; t <= maxTemporal; t++ {
				BandwidthRequested := brs[s][t]
				if BandwidthRequested == 0 {
					continue
				}

				if !allowOvershoot && BandwidthRequested-alreadyAllocated > availableChannelCapacity {
					// next higher available layer does not fit, return
					return true, f.lastAllocation, false
				}

				targetLayers := buffer.VideoLayer{Spatial: s, Temporal: t}
				alloc := VideoAllocation{
					IsDeficient:         true,
					BandwidthRequested:  BandwidthRequested,
					BandwidthDelta:      BandwidthRequested - alreadyAllocated,
					BandwidthNeeded:     optimalBandwidthNeeded,
					Bitrates:            brs,
					TargetLayers:        targetLayers,
					RequestLayerSpatial: targetLayers.Spatial,
					MaxLayers:           f.maxLayers,
					DistanceToDesired: getDistanceToDesired(
						f.muted,
						f.pubMuted,
						f.maxPublishedLayer,
						f.maxTemporalLayerSeen,
						availableLayers,
						brs,
						targetLayers,
						f.maxLayers,
					),
				}
				if targetLayers.GreaterThan(f.maxLayers) || BandwidthRequested >= optimalBandwidthNeeded {
					alloc.IsDeficient = false
				}

				return true, f.updateAllocation(alloc, "next-higher"), true
			}
		}

		return false, VideoAllocation{}, false
	}

	done := false
	var allocation VideoAllocation
	boosted := false

	// try moving temporal layer up in currently streaming spatial layer
	if f.targetLayers.IsValid() {
		done, allocation, boosted = doAllocation(
			f.targetLayers.Spatial, f.targetLayers.Spatial,
			f.targetLayers.Temporal+1, f.maxLayers.Temporal,
		)
		if done {
			return allocation, boosted
		}
	}

	// try moving spatial layer up if temporal layer move up is not available
	done, allocation, boosted = doAllocation(
		f.targetLayers.Spatial+1, f.maxLayers.Spatial,
		0, f.maxLayers.Temporal,
	)
	if done {
		return allocation, boosted
	}

	if allowOvershoot && f.maxLayers.IsValid() {
		done, allocation, boosted = doAllocation(
			f.maxLayers.Spatial+1, buffer.DefaultMaxLayerSpatial,
			0, buffer.DefaultMaxLayerTemporal,
		)
		if done {
			return allocation, boosted
		}
	}

	return f.lastAllocation, false
}

func (f *Forwarder) GetNextHigherTransition(brs Bitrates, allowOvershoot bool) (VideoTransition, bool) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.kind == webrtc.RTPCodecTypeAudio {
		return VideoTransition{}, false
	}

	// if not deficient, nothing to do
	if !f.isDeficientLocked() {
		return VideoTransition{}, false
	}

	// if targets are still pending, don't increase
	if f.targetLayers.IsValid() && f.targetLayers != f.currentLayers {
		return VideoTransition{}, false
	}

	alreadyAllocated := int64(0)
	if f.targetLayers.IsValid() {
		alreadyAllocated = brs[f.targetLayers.Spatial][f.targetLayers.Temporal]
	}

	findNextHigher := func(
		minSpatial, maxSpatial int32,
		minTemporal, maxTemporal int32,
	) (bool, VideoTransition, bool) {
		for s := minSpatial; s <= maxSpatial; s++ {
			for t := minTemporal; t <= maxTemporal; t++ {
				BandwidthRequested := brs[s][t]
				if BandwidthRequested == 0 {
					continue
				}

				transition := VideoTransition{
					From:           f.targetLayers,
					To:             buffer.VideoLayer{Spatial: s, Temporal: t},
					BandwidthDelta: BandwidthRequested - alreadyAllocated,
				}

				return true, transition, true
			}
		}

		return false, VideoTransition{}, false
	}

	done := false
	var transition VideoTransition
	isAvailable := false

	// try moving temporal layer up in currently streaming spatial layer
	if f.targetLayers.IsValid() {
		done, transition, isAvailable = findNextHigher(
			f.targetLayers.Spatial, f.targetLayers.Spatial,
			f.targetLayers.Temporal+1, f.maxLayers.Temporal,
		)
		if done {
			return transition, isAvailable
		}
	}

	// try moving spatial layer up if temporal layer move up is not available
	done, transition, isAvailable = findNextHigher(
		f.targetLayers.Spatial+1, f.maxLayers.Spatial,
		0, f.maxLayers.Temporal,
	)
	if done {
		return transition, isAvailable
	}

	if allowOvershoot && f.maxLayers.IsValid() {
		done, transition, isAvailable = findNextHigher(
			f.maxLayers.Spatial+1, buffer.DefaultMaxLayerSpatial,
			0, buffer.DefaultMaxLayerTemporal,
		)
		if done {
			return transition, isAvailable
		}
	}

	return VideoTransition{}, false
}

func (f *Forwarder) Pause(availableLayers []int32, brs Bitrates) VideoAllocation {
	f.lock.Lock()
	defer f.lock.Unlock()

	optimalBandwidthNeeded := getOptimalBandwidthNeeded(f.muted, f.pubMuted, f.maxPublishedLayer, brs, f.maxLayers)
	alloc := VideoAllocation{
		BandwidthRequested:  0,
		BandwidthDelta:      0 - f.lastAllocation.BandwidthRequested,
		Bitrates:            brs,
		BandwidthNeeded:     optimalBandwidthNeeded,
		TargetLayers:        buffer.InvalidLayers,
		RequestLayerSpatial: buffer.InvalidLayerSpatial,
		MaxLayers:           f.maxLayers,
		DistanceToDesired: getDistanceToDesired(
			f.muted,
			f.pubMuted,
			f.maxPublishedLayer,
			f.maxTemporalLayerSeen,
			availableLayers,
			brs,
			buffer.InvalidLayers,
			f.maxLayers,
		),
	}

	switch {
	case f.muted:
		alloc.PauseReason = VideoPauseReasonMuted

	case f.pubMuted:
		alloc.PauseReason = VideoPauseReasonPubMuted

	case optimalBandwidthNeeded == 0:
		alloc.PauseReason = VideoPauseReasonFeedDry

	default:
		// pausing due to lack of bandwidth
		alloc.IsDeficient = true
		alloc.PauseReason = VideoPauseReasonBandwidth
	}

	f.clearParkedLayers()
	return f.updateAllocation(alloc, "pause")
}

func (f *Forwarder) updateAllocation(alloc VideoAllocation, reason string) VideoAllocation {
	if alloc.IsDeficient != f.lastAllocation.IsDeficient ||
		alloc.PauseReason != f.lastAllocation.PauseReason ||
		alloc.TargetLayers != f.lastAllocation.TargetLayers ||
		alloc.RequestLayerSpatial != f.lastAllocation.RequestLayerSpatial {
		if reason == "optimal" {
			f.logger.Debugw(fmt.Sprintf("stream allocation: %s", reason), "allocation", alloc)
		} else {
			f.logger.Infow(fmt.Sprintf("stream allocation: %s", reason), "allocation", alloc)
		}
	}
	f.lastAllocation = alloc

	f.setTargetLayers(f.lastAllocation.TargetLayers, f.lastAllocation.RequestLayerSpatial)
	if !f.targetLayers.IsValid() {
		f.resyncLocked()
	}

	return f.lastAllocation
}

func (f *Forwarder) setTargetLayers(targetLayers buffer.VideoLayer, requestLayerSpatial int32) {
	f.targetLayers = targetLayers
	if f.ddLayerSelector != nil {
		f.ddLayerSelector.SelectLayer(targetLayers)
	}

	f.requestLayerSpatial = requestLayerSpatial
}

func (f *Forwarder) Resync() {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.resyncLocked()
}

func (f *Forwarder) resyncLocked() {
	f.currentLayers = buffer.InvalidLayers
	f.lastSSRC = 0
	f.clearParkedLayers()
}

func (f *Forwarder) clearParkedLayers() {
	f.parkedLayers = buffer.InvalidLayers
	if f.parkedLayersTimer != nil {
		f.parkedLayersTimer.Stop()
		f.parkedLayersTimer = nil
	}
}

func (f *Forwarder) setupParkedLayers(parkedLayers buffer.VideoLayer) {
	f.clearParkedLayers()

	f.parkedLayers = parkedLayers
	f.parkedLayersTimer = time.AfterFunc(ParkedLayersWaitDuration, func() {
		f.lock.Lock()
		f.clearParkedLayers()
		f.lock.Unlock()

		if onParkedLayersExpired := f.getOnParkedLayersExpired(); onParkedLayersExpired != nil {
			onParkedLayersExpired()
		}
	})
}

func (f *Forwarder) CheckSync() (locked bool, layer int32) {
	f.lock.RLock()
	defer f.lock.RUnlock()

	layer = f.requestLayerSpatial
	locked = f.requestLayerSpatial == f.currentLayers.Spatial || f.parkedLayers.IsValid()
	return
}

func (f *Forwarder) FilterRTX(nacks []uint16) (filtered []uint16, disallowedLayers [buffer.DefaultMaxLayerSpatial + 1]bool) {
	if !FlagFilterRTX {
		filtered = nacks
		return
	}

	f.lock.RLock()
	defer f.lock.RUnlock()

	filtered = f.rtpMunger.FilterRTX(nacks)

	//
	// Curb RTX when deficient for two cases
	//   1. Target layer is lower than current layer. When current hits target, a key frame should flush the decoder.
	//   2. Requested layer is higher than current. Current layer's key frame should have flushed encoder.
	//      Remote might ask for older layer because of its jitter buffer, but let it starve as channel is already congested.
	//
	// Without the curb, when congestion hits, RTX rate could be so high that it further congests the channel.
	//
	for layer := int32(0); layer < buffer.DefaultMaxLayerSpatial+1; layer++ {
		if f.isDeficientLocked() && (f.targetLayers.Spatial < f.currentLayers.Spatial || layer > f.currentLayers.Spatial) {
			disallowedLayers[layer] = true
		}
	}

	return
}

func (f *Forwarder) GetTranslationParams(extPkt *buffer.ExtPacket, layer int32) (*TranslationParams, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	// Video: Do not drop on publisher mute to enable resume on publisher unmute without a key frame.
	if f.muted {
		return &TranslationParams{
			shouldDrop: true,
		}, nil
	}

	switch f.kind {
	case webrtc.RTPCodecTypeAudio:
		// Audio: Blank frames are injected on publisher mute to ensure decoder does not get stuck at a noise frame. So, do not forward.
		if f.pubMuted {
			return &TranslationParams{
				shouldDrop: true,
			}, nil
		}

		return f.getTranslationParamsAudio(extPkt, layer)
	case webrtc.RTPCodecTypeVideo:
		return f.getTranslationParamsVideo(extPkt, layer)
	}

	return nil, ErrUnknownKind
}

// should be called with lock held
func (f *Forwarder) getTranslationParamsCommon(extPkt *buffer.ExtPacket, layer int32, tp *TranslationParams) (*TranslationParams, error) {
	if f.lastSSRC != extPkt.Packet.SSRC {
		if !f.started {
			f.started = true
			f.referenceLayerSpatial = layer
			f.rtpMunger.SetLastSnTs(extPkt)
			if f.vp8Munger != nil {
				f.vp8Munger.SetLast(extPkt)
			}
		} else {
			if f.referenceLayerSpatial == buffer.InvalidLayerSpatial {
				// on a resume, reference layer may not be set, so only set when it is invalid
				f.referenceLayerSpatial = layer
			}

			// Compute how much time passed between the old RTP extPkt
			// and the current packet, and fix timestamp on source change
			td := uint32(1)
			if f.getReferenceLayerRTPTimestamp != nil {
				refTS, err := f.getReferenceLayerRTPTimestamp(extPkt.Packet.Timestamp, layer, f.referenceLayerSpatial)
				if err == nil {
					last := f.rtpMunger.GetLast()
					td = refTS - last.LastTS
					if td == 0 || td > (1<<31) {
						f.logger.Debugw("reference timestamp out-of-order, using default", "lastTS", last.LastTS, "refTS", refTS, "td", int32(td))
						td = 1
					} else if td > uint32(0.5*float32(f.codec.ClockRate)) {
						// log jumps greater than 0.5 seconds
						f.logger.Debugw("reference timestamp too far ahead", "lastTS", last.LastTS, "refTS", refTS, "td", td)
					}
				} else {
					f.logger.Debugw("reference timestamp get error, using default", "error", err)
				}
			}

			f.rtpMunger.UpdateSnTsOffsets(extPkt, 1, td)
			if f.vp8Munger != nil {
				f.vp8Munger.UpdateOffsets(extPkt)
			}
		}

		f.logger.Debugw("switching feed", "from", f.lastSSRC, "to", extPkt.Packet.SSRC)
		f.lastSSRC = extPkt.Packet.SSRC
	}

	if tp == nil {
		tp = &TranslationParams{}
	}
	tpRTP, err := f.rtpMunger.UpdateAndGetSnTs(extPkt)
	if err != nil {
		tp.shouldDrop = true
		if err == ErrPaddingOnlyPacket || err == ErrDuplicatePacket || err == ErrOutOfOrderSequenceNumberCacheMiss {
			if err == ErrOutOfOrderSequenceNumberCacheMiss {
				tp.isDroppingRelevant = true
			}
			return tp, nil
		}

		tp.isDroppingRelevant = true
		return tp, err
	}

	tp.rtp = tpRTP
	return tp, nil
}

// should be called with lock held
func (f *Forwarder) getTranslationParamsAudio(extPkt *buffer.ExtPacket, layer int32) (*TranslationParams, error) {
	return f.getTranslationParamsCommon(extPkt, layer, nil)
}

// should be called with lock held
func (f *Forwarder) getTranslationParamsVideo(extPkt *buffer.ExtPacket, layer int32) (*TranslationParams, error) {
	tp := &TranslationParams{}

	if !f.targetLayers.IsValid() {
		// stream is paused by streamallocator
		tp.shouldDrop = true
		return tp, nil
	}

	if f.ddLayerSelector != nil {
		if selected := f.ddLayerSelector.Select(extPkt, tp); !selected {
			tp.shouldDrop = true
			f.rtpMunger.UpdateAndGetSnTs(extPkt) // call to update highest incoming sequence number and other internal structures
			f.rtpMunger.PacketDropped(extPkt)
			return tp, nil
		}
	}

	// at this point, either
	// 1. dependency description has selected the layer for forwarding OR
	// 2. non-dependency deescriptor is yet to make decision, but it can potentially switch to the incoming layer and start forwarding
	//
	// both cases cases upgrade/downgrade to current layer under the right conditions
	if f.currentLayers.Spatial != f.targetLayers.Spatial {
		// Three things to check when not locked to target
		//   1. Resumable layer - don't need a key frame
		//   2. Opportunistic layer upgrade - needs a key frame if not using depedency descriptor
		//   3. Need to downgrade - needs a key frame if not using dependency descriptor
		found := false
		if f.parkedLayers.IsValid() {
			if f.parkedLayers.Spatial == layer {
				f.logger.Infow(
					"resuming at parked layer",
					"current", f.currentLayers,
					"target", f.targetLayers,
					"parked", f.parkedLayers,
					"feed", extPkt.Packet.SSRC,
				)
				f.currentLayers = f.parkedLayers
				found = true
			}
		} else {
			if extPkt.KeyFrame || tp.isSwitchingToTargetLayer {
				if layer > f.currentLayers.Spatial && layer <= f.targetLayers.Spatial {
					f.logger.Infow(
						"upgrading layer",
						"current", f.currentLayers,
						"target", f.targetLayers,
						"max", f.maxLayers,
						"layer", layer,
						"req", f.requestLayerSpatial,
						"maxPublished", f.maxPublishedLayer,
						"feed", extPkt.Packet.SSRC,
					)
					found = true
				}

				if layer < f.currentLayers.Spatial && layer >= f.targetLayers.Spatial {
					f.logger.Infow(
						"downgrading layer",
						"current", f.currentLayers,
						"target", f.targetLayers,
						"max", f.maxLayers,
						"layer", layer,
						"req", f.requestLayerSpatial,
						"maxPublished", f.maxPublishedLayer,
						"feed", extPkt.Packet.SSRC,
					)
					found = true
				}

				if found {
					f.currentLayers.Spatial = layer
					if !f.isTemporalSupported {
						f.currentLayers.Temporal = f.targetLayers.Temporal
					}
				}
			}
		}

		if found {
			tp.isSwitchingToTargetLayer = true
			f.clearParkedLayers()
			if f.currentLayers.Spatial >= f.maxLayers.Spatial {
				tp.isSwitchingToMaxLayer = true

				f.logger.Infow(
					"reached max layer",
					"current", f.currentLayers,
					"target", f.targetLayers,
					"max", f.maxLayers,
					"layer", layer,
					"req", f.requestLayerSpatial,
					"maxPublished", f.maxPublishedLayer,
					"feed", extPkt.Packet.SSRC,
				)
			}

			if f.currentLayers.Spatial >= f.maxLayers.Spatial || f.currentLayers.Spatial == f.maxPublishedLayer {
				f.targetLayers.Spatial = f.currentLayers.Spatial
				if f.ddLayerSelector != nil {
					f.ddLayerSelector.SelectLayer(f.targetLayers)
				}
			}
		}
	}

	// if locked to higher than max layer due to overshoot, check if it can be dialed back
	if f.currentLayers.Spatial > f.maxLayers.Spatial {
		if layer <= f.maxLayers.Spatial && (extPkt.KeyFrame || tp.isSwitchingToTargetLayer) {
			f.logger.Infow(
				"adjusting overshoot",
				"current", f.currentLayers,
				"target", f.targetLayers,
				"max", f.maxLayers,
				"layer", layer,
				"req", f.requestLayerSpatial,
				"maxPublished", f.maxPublishedLayer,
				"feed", extPkt.Packet.SSRC,
			)
			f.currentLayers.Spatial = layer

			if f.currentLayers.Spatial >= f.maxLayers.Spatial {
				tp.isSwitchingToMaxLayer = true
			}

			if f.currentLayers.Spatial >= f.maxLayers.Spatial || f.currentLayers.Spatial == f.maxPublishedLayer {
				f.targetLayers.Spatial = layer
				if f.ddLayerSelector != nil {
					f.ddLayerSelector.SelectLayer(f.targetLayers)
				}
			}
		}
	}

	// if we have layer selector, let it decide whether to drop or not
	if f.ddLayerSelector == nil && f.currentLayers.Spatial != layer {
		tp.shouldDrop = true
		return tp, nil
	}

	if FlagPauseOnDowngrade && f.targetLayers.Spatial < f.currentLayers.Spatial && f.isDeficientLocked() {
		//
		// If target layer is lower than both the current and
		// maximum subscribed layer, it is due to bandwidth
		// constraints that the target layer has been switched down.
		// Continuing to send higher layer will only exacerbate the
		// situation by putting more stress on the channel. So, drop it.
		//
		// In the other direction, it is okay to keep forwarding till
		// switch point to get a smoother stream till the higher
		// layer key frame arrives.
		//
		// Note that it is possible for client subscription layer restriction
		// to coincide with server restriction due to bandwidth limitation,
		// In the case of subscription change, higher should continue streaming
		// to ensure smooth transition.
		//
		// To differentiate between the two cases, drop only when in DEFICIENT state.
		//
		tp.shouldDrop = true
		tp.isDroppingRelevant = true
		return tp, nil
	}

	_, err := f.getTranslationParamsCommon(extPkt, layer, tp)
	if tp.shouldDrop || f.vp8Munger == nil || len(extPkt.Packet.Payload) == 0 {
		return tp, err
	}

	// catch up temporal layer if necessary
	if f.currentLayers.Temporal != f.targetLayers.Temporal {
		incomingVP8, ok := extPkt.Payload.(buffer.VP8)
		if ok {
			if incomingVP8.TIDPresent == 0 || incomingVP8.TID <= uint8(f.targetLayers.Temporal) {
				f.currentLayers.Temporal = f.targetLayers.Temporal
			}
		}
	}

	tpVP8, err := f.vp8Munger.UpdateAndGet(extPkt, tp.rtp.snOrdering, f.currentLayers.Temporal)
	if err != nil {
		tp.rtp = nil
		tp.shouldDrop = true
		if err == ErrFilteredVP8TemporalLayer || err == ErrOutOfOrderVP8PictureIdCacheMiss {
			if err == ErrFilteredVP8TemporalLayer {
				// filtered temporal layer, update sequence number offset to prevent holes
				f.rtpMunger.PacketDropped(extPkt)
			}
			if err == ErrOutOfOrderVP8PictureIdCacheMiss {
				tp.isDroppingRelevant = true
			}
			return tp, nil
		}

		tp.isDroppingRelevant = true
		return tp, err
	}

	tp.vp8 = tpVP8
	return tp, nil
}

func (f *Forwarder) GetSnTsForPadding(num int) ([]SnTs, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	// padding is used for probing. Padding packets should be
	// at only the frame boundaries to ensure decoder sequencer does
	// not get out-of-sync. But, when a stream is paused,
	// force a frame marker as a restart of the stream will
	// start with a key frame which will reset the decoder.
	forceMarker := false
	if !f.targetLayers.IsValid() {
		forceMarker = true
	}
	return f.rtpMunger.UpdateAndGetPaddingSnTs(num, 0, 0, forceMarker)
}

func (f *Forwarder) GetSnTsForBlankFrames(frameRate uint32, numPackets int) ([]SnTs, bool, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	frameEndNeeded := !f.rtpMunger.IsOnFrameBoundary()
	if frameEndNeeded {
		numPackets++
	}
	snts, err := f.rtpMunger.UpdateAndGetPaddingSnTs(numPackets, f.codec.ClockRate, frameRate, frameEndNeeded)
	return snts, frameEndNeeded, err
}

func (f *Forwarder) GetPaddingVP8(frameEndNeeded bool) *buffer.VP8 {
	f.lock.Lock()
	defer f.lock.Unlock()

	return f.vp8Munger.UpdateAndGetPadding(!frameEndNeeded)
}

func (f *Forwarder) GetRTPMungerParams() RTPMungerParams {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.rtpMunger.GetParams()
}

// -----------------------------------------------------------------------------

func getOptimalBandwidthNeeded(muted bool, pubMuted bool, maxPublishedLayer int32, brs Bitrates, maxLayers buffer.VideoLayer) int64 {
	if muted || pubMuted || maxPublishedLayer == buffer.InvalidLayerSpatial {
		return 0
	}

	for i := maxLayers.Spatial; i >= 0; i-- {
		for j := maxLayers.Temporal; j >= 0; j-- {
			if brs[i][j] == 0 {
				continue
			}

			return brs[i][j]
		}
	}

	// could be 0 due to either
	//   1. publisher has stopped all layers ==> feed dry.
	//   2. stream tracker has declared all layers stopped, functionally same as above.
	//      But, listed differently as this could be a mis-detection.
	//   3. Bitrate measurement is pending.
	return 0
}

func getDistanceToDesired(
	muted bool,
	pubMuted bool,
	maxPublishedLayer int32,
	maxTemporalLayerSeen int32,
	availableLayers []int32,
	brs Bitrates,
	targetLayers buffer.VideoLayer,
	maxLayers buffer.VideoLayer,
) float64 {
	if muted || pubMuted || maxPublishedLayer == buffer.InvalidLayerSpatial || maxTemporalLayerSeen == buffer.InvalidLayerTemporal || !maxLayers.IsValid() {
		return 0.0
	}

	adjustedMaxLayers := maxLayers

	maxAvailableSpatial := buffer.InvalidLayerSpatial
	maxAvailableTemporal := buffer.InvalidLayerTemporal

	// max available spatial is min(subscribedMax, publishedMax, availableMax)
	// subscribedMax = subscriber requested max spatial layer
	// publishedMax = max spatial layer ever published
	// availableMax = based on bit rate measurement, available max spatial layer
done:
	for s := int32(len(brs)) - 1; s >= 0; s-- {
		for t := int32(len(brs[0])) - 1; t >= 0; t-- {
			if brs[s][t] != 0 {
				maxAvailableSpatial = s
				break done
			}
		}
	}

	// before bit rate measurement is available, stream tracker could declare layer seen, account for that
	for _, layer := range availableLayers {
		if layer > maxAvailableSpatial {
			maxAvailableSpatial = layer
			maxAvailableTemporal = maxTemporalLayerSeen // till bit rate measurement is available, assume max seen as temporal
		}
	}

	if maxAvailableSpatial < adjustedMaxLayers.Spatial {
		adjustedMaxLayers.Spatial = maxAvailableSpatial
	}

	if maxPublishedLayer < adjustedMaxLayers.Spatial {
		adjustedMaxLayers.Spatial = maxPublishedLayer
	}

	// max available temporal is min(subscribedMax, temporalLayerSeenMax, availableMax)
	// subscribedMax = subscriber requested max temporal layer
	// temporalLayerSeenMax = max temporal layer ever published/seen
	// availableMax = based on bit rate measurement, available max temporal in the adjusted max spatial layer
	if adjustedMaxLayers.Spatial != buffer.InvalidLayerSpatial {
		for t := int32(len(brs[0])) - 1; t >= 0; t-- {
			if brs[adjustedMaxLayers.Spatial][t] != 0 {
				maxAvailableTemporal = t
				break
			}
		}
	}
	if maxAvailableTemporal < adjustedMaxLayers.Temporal {
		adjustedMaxLayers.Temporal = maxAvailableTemporal
	}

	if maxTemporalLayerSeen < adjustedMaxLayers.Temporal {
		adjustedMaxLayers.Temporal = maxTemporalLayerSeen
	}

	if !adjustedMaxLayers.IsValid() {
		adjustedMaxLayers = buffer.VideoLayer{Spatial: 0, Temporal: 0}
	}

	// adjust target layers if they are invalid, i. e. not streaming
	adjustedTargetLayers := targetLayers
	if !targetLayers.IsValid() {
		adjustedTargetLayers = buffer.VideoLayer{Spatial: 0, Temporal: 0}
	}

	distance :=
		((adjustedMaxLayers.Spatial - adjustedTargetLayers.Spatial) * (maxTemporalLayerSeen + 1)) +
			(adjustedMaxLayers.Temporal - adjustedTargetLayers.Temporal)
	if !targetLayers.IsValid() {
		distance++
	}

	return float64(distance) / float64(maxTemporalLayerSeen+1)
}
