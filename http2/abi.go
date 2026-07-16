package http2

import (
	"encoding/binary"
	"errors"
	"math"
	"sync"

	wagonet "github.com/wago-org/net"
	wago "github.com/wago-org/wago"
)

const (
	FeatureSessionEngine uint64 = 1 << iota
	FeatureMultiplexedStreams
	FeatureServer
	FeaturePush
	FeatureExtendedConnect
	FeaturePriorityUpdate
)

const FeatureAll = FeatureSessionEngine | FeatureMultiplexedStreams | FeatureServer | FeaturePush | FeatureExtendedConnect | FeaturePriorityUpdate

const (
	ABIHeaderV1Size  uint32 = 24
	ABIEventV1Size   uint32 = 32
	ABISettingV1Size uint32 = 8

	ABIFlagEndStream uint32 = 1
	ABIFlagSensitive uint32 = 1
)

type registerConfig struct {
	session     SessionLimits
	maxSessions uint32
}

// Option configures HTTP/2 guest session resources.
type Option interface{ applyHTTP2(*registerConfig) }

type optionFunc func(*registerConfig)

func (option optionFunc) applyHTTP2(config *registerConfig) { option(config) }

// WithSessionLimits configures every guest-created HTTP/2 session.
func WithSessionLimits(limits SessionLimits) Option {
	return optionFunc(func(config *registerConfig) { config.session = limits })
}

// WithMaxSessions bounds live HTTP/2 session handles per Wago instance.
func WithMaxSessions(max uint32) Option {
	return optionFunc(func(config *registerConfig) { config.maxSessions = max })
}

type abiManager struct {
	mu        sync.Mutex
	config    registerConfig
	instances map[*wago.Instance]*abiInstance
}

type abiInstance struct {
	next     uint32
	sessions map[uint32]*abiSession
}

type abiSession struct {
	mu         sync.Mutex
	session    *Session
	current    Event
	hasCurrent bool
}

func newABIManager(config registerConfig) *abiManager {
	if config.maxSessions == 0 {
		config.maxSessions = 8
	}
	config.session = config.session.normalized()
	return &abiManager{config: config, instances: make(map[*wago.Instance]*abiInstance)}
}

func (manager *abiManager) configure(registry *wago.Registry) {
	registry.RequireReinstantiation()
	registry.Hooks().BeforeClose(func(context *wago.InstanceContext) {
		manager.closeInstance(context.Instance)
	})
	registry.Hooks().OnRuntimeClose(func(*wago.RuntimeContext) {
		manager.closeAll()
	})
}

func (manager *abiManager) closeInstance(instance *wago.Instance) {
	if instance == nil {
		return
	}
	manager.mu.Lock()
	state := manager.instances[instance]
	delete(manager.instances, instance)
	manager.mu.Unlock()
	if state != nil {
		for _, session := range state.sessions {
			session.mu.Lock()
			session.session.Close()
			session.mu.Unlock()
		}
	}
}

func (manager *abiManager) closeAll() {
	manager.mu.Lock()
	instances := manager.instances
	manager.instances = make(map[*wago.Instance]*abiInstance)
	manager.mu.Unlock()
	for _, state := range instances {
		for _, session := range state.sessions {
			session.mu.Lock()
			session.session.Close()
			session.mu.Unlock()
		}
	}
}

func (manager *abiManager) caller(host wago.HostModule) (*abiInstance, []byte, wagonet.Status) {
	identified, ok := host.(wago.InstanceHostModule)
	if !ok || identified.Instance() == nil {
		return nil, nil, wagonet.StatusInvalidState
	}
	instance := identified.Instance()
	manager.mu.Lock()
	state := manager.instances[instance]
	if state == nil {
		state = &abiInstance{next: 1, sessions: make(map[uint32]*abiSession)}
		manager.instances[instance] = state
	}
	manager.mu.Unlock()
	return state, host.Memory(), wagonet.StatusOK
}

func (manager *abiManager) sessionOpen(host wago.HostModule, params, results []uint64) {
	if len(params) != 2 || len(results) != 1 {
		return
	}
	state, memory, status := manager.caller(host)
	if status != wagonet.StatusOK {
		results[0] = uint64(status)
		return
	}
	role := Role(uint32(params[0]))
	out, ok := abiMemory(memory, uint32(params[1]), 4)
	if !ok {
		results[0] = uint64(wagonet.StatusInvalidArgument)
		return
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if uint32(len(state.sessions)) >= manager.config.maxSessions {
		results[0] = uint64(wagonet.StatusResourceLimit)
		return
	}
	session, err := NewSession(role, manager.config.session)
	if err != nil {
		results[0] = uint64(statusForError(err))
		return
	}
	handle := state.next
	for handle == 0 || state.sessions[handle] != nil {
		handle++
		if handle == state.next {
			session.Close()
			results[0] = uint64(wagonet.StatusResourceLimit)
			return
		}
	}
	state.next = handle + 1
	state.sessions[handle] = &abiSession{session: session}
	binary.LittleEndian.PutUint32(out, handle)
	results[0] = uint64(wagonet.StatusOK)
}

func (manager *abiManager) sessionClose(host wago.HostModule, params, results []uint64) {
	if len(params) != 1 || len(results) != 1 {
		return
	}
	state, _, status := manager.caller(host)
	if status != wagonet.StatusOK {
		results[0] = uint64(status)
		return
	}
	handle := uint32(params[0])
	manager.mu.Lock()
	session := state.sessions[handle]
	delete(state.sessions, handle)
	manager.mu.Unlock()
	if session == nil {
		results[0] = uint64(wagonet.StatusBadHandle)
		return
	}
	session.mu.Lock()
	session.session.Close()
	session.mu.Unlock()
	results[0] = uint64(wagonet.StatusOK)
}

func (manager *abiManager) sessionFeed(host wago.HostModule, params, results []uint64) {
	if len(params) != 4 || len(results) != 1 {
		return
	}
	session, memory, status := manager.lookup(host, uint32(params[0]))
	if status == wagonet.StatusOK {
		session.mu.Lock()
		defer session.mu.Unlock()
	}
	if status != wagonet.StatusOK {
		results[0] = uint64(status)
		return
	}
	src, ok := abiMemory(memory, uint32(params[1]), uint32(params[2]))
	consumed, consumedOK := abiMemory(memory, uint32(params[3]), 4)
	if !ok || !consumedOK {
		results[0] = uint64(wagonet.StatusInvalidArgument)
		return
	}
	n, err := session.session.Feed(src)
	binary.LittleEndian.PutUint32(consumed, uint32(n))
	results[0] = uint64(statusForError(err))
}

func (manager *abiManager) sessionOutput(host wago.HostModule, params, results []uint64) {
	if len(params) != 4 || len(results) != 1 {
		return
	}
	session, memory, status := manager.lookup(host, uint32(params[0]))
	if status == wagonet.StatusOK {
		session.mu.Lock()
		defer session.mu.Unlock()
	}
	if status != wagonet.StatusOK {
		results[0] = uint64(status)
		return
	}
	dst, ok := abiMemory(memory, uint32(params[1]), uint32(params[2]))
	written, writtenOK := abiMemory(memory, uint32(params[3]), 4)
	if !ok || !writtenOK {
		results[0] = uint64(wagonet.StatusInvalidArgument)
		return
	}
	if len(session.session.Output()) == 0 {
		binary.LittleEndian.PutUint32(written, 0)
		results[0] = uint64(wagonet.StatusAgain)
		return
	}
	n := copy(dst, session.session.Output())
	_ = session.session.ConsumeOutput(n)
	binary.LittleEndian.PutUint32(written, uint32(n))
	results[0] = uint64(wagonet.StatusOK)
}

func (manager *abiManager) streamOpen(host wago.HostModule, params, results []uint64) {
	if len(params) != 5 || len(results) != 1 {
		return
	}
	session, memory, status := manager.lookup(host, uint32(params[0]))
	if status == wagonet.StatusOK {
		session.mu.Lock()
		defer session.mu.Unlock()
	}
	if status != wagonet.StatusOK {
		results[0] = uint64(status)
		return
	}
	headers, status := readABIHeaders(memory, uint32(params[1]), uint32(params[2]), session.session.limits.Header)
	out, ok := abiMemory(memory, uint32(params[4]), 4)
	if status != wagonet.StatusOK || !ok {
		if !ok {
			status = wagonet.StatusInvalidArgument
		}
		results[0] = uint64(status)
		return
	}
	streamID, err := session.session.OpenStream(headers, uint32(params[3])&ABIFlagEndStream != 0)
	if err == nil {
		binary.LittleEndian.PutUint32(out, streamID)
	}
	results[0] = uint64(statusForError(err))
}

func (manager *abiManager) streamHeaders(host wago.HostModule, params, results []uint64) {
	if len(params) != 5 || len(results) != 1 {
		return
	}
	session, memory, status := manager.lookup(host, uint32(params[0]))
	if status == wagonet.StatusOK {
		session.mu.Lock()
		defer session.mu.Unlock()
	}
	if status != wagonet.StatusOK {
		results[0] = uint64(status)
		return
	}
	headers, status := readABIHeaders(memory, uint32(params[2]), uint32(params[3]), session.session.limits.Header)
	if status != wagonet.StatusOK {
		results[0] = uint64(status)
		return
	}
	err := session.session.SendHeaders(uint32(params[1]), headers, uint32(params[4])&ABIFlagEndStream != 0)
	results[0] = uint64(statusForError(err))
}

func (manager *abiManager) streamData(host wago.HostModule, params, results []uint64) {
	if len(params) != 6 || len(results) != 1 {
		return
	}
	session, memory, status := manager.lookup(host, uint32(params[0]))
	if status == wagonet.StatusOK {
		session.mu.Lock()
		defer session.mu.Unlock()
	}
	if status != wagonet.StatusOK {
		results[0] = uint64(status)
		return
	}
	src, ok := abiMemory(memory, uint32(params[2]), uint32(params[3]))
	consumed, consumedOK := abiMemory(memory, uint32(params[5]), 4)
	if !ok || !consumedOK {
		results[0] = uint64(wagonet.StatusInvalidArgument)
		return
	}
	n, err := session.session.SendData(uint32(params[1]), src, uint32(params[4])&ABIFlagEndStream != 0)
	binary.LittleEndian.PutUint32(consumed, uint32(n))
	results[0] = uint64(statusForError(err))
}

func (manager *abiManager) streamPush(host wago.HostModule, params, results []uint64) {
	if len(params) != 5 || len(results) != 1 {
		return
	}
	session, memory, status := manager.lookup(host, uint32(params[0]))
	if status == wagonet.StatusOK {
		session.mu.Lock()
		defer session.mu.Unlock()
	}
	if status != wagonet.StatusOK {
		results[0] = uint64(status)
		return
	}
	headers, status := readABIHeaders(memory, uint32(params[2]), uint32(params[3]), session.session.limits.Header)
	out, ok := abiMemory(memory, uint32(params[4]), 4)
	if status != wagonet.StatusOK || !ok {
		if !ok {
			status = wagonet.StatusInvalidArgument
		}
		results[0] = uint64(status)
		return
	}
	streamID, err := session.session.PushPromise(uint32(params[1]), headers)
	if err == nil {
		binary.LittleEndian.PutUint32(out, streamID)
	}
	results[0] = uint64(statusForError(err))
}

func (manager *abiManager) sessionSettings(host wago.HostModule, params, results []uint64) {
	if len(params) != 3 || len(results) != 1 {
		return
	}
	session, memory, status := manager.lookup(host, uint32(params[0]))
	if status == wagonet.StatusOK {
		session.mu.Lock()
		defer session.mu.Unlock()
	}
	if status != wagonet.StatusOK {
		results[0] = uint64(status)
		return
	}
	count := uint32(params[2])
	if count > 64 {
		results[0] = uint64(wagonet.StatusResourceLimit)
		return
	}
	encoded, ok := abiMemory(memory, uint32(params[1]), count*ABISettingV1Size)
	if !ok {
		results[0] = uint64(wagonet.StatusInvalidArgument)
		return
	}
	settings := make([]Setting, count)
	for index := uint32(0); index < count; index++ {
		entry := encoded[index*ABISettingV1Size : (index+1)*ABISettingV1Size]
		settings[index] = Setting{ID: SettingID(binary.LittleEndian.Uint32(entry[:4])), Value: binary.LittleEndian.Uint32(entry[4:8])}
	}
	results[0] = uint64(statusForError(session.session.UpdateSettings(settings)))
}

func (manager *abiManager) sessionPing(host wago.HostModule, params, results []uint64) {
	if len(params) != 2 || len(results) != 1 {
		return
	}
	session, memory, status := manager.lookup(host, uint32(params[0]))
	if status == wagonet.StatusOK {
		session.mu.Lock()
		defer session.mu.Unlock()
	}
	if status != wagonet.StatusOK {
		results[0] = uint64(status)
		return
	}
	data, ok := abiMemory(memory, uint32(params[1]), 8)
	if !ok {
		results[0] = uint64(wagonet.StatusInvalidArgument)
		return
	}
	var ping [8]byte
	copy(ping[:], data)
	results[0] = uint64(statusForError(session.session.Ping(ping)))
}

func (manager *abiManager) sessionGoAway(host wago.HostModule, params, results []uint64) {
	if len(params) != 4 || len(results) != 1 {
		return
	}
	session, memory, status := manager.lookup(host, uint32(params[0]))
	if status == wagonet.StatusOK {
		session.mu.Lock()
		defer session.mu.Unlock()
	}
	if status != wagonet.StatusOK {
		results[0] = uint64(status)
		return
	}
	debug, ok := abiMemory(memory, uint32(params[2]), uint32(params[3]))
	if !ok {
		results[0] = uint64(wagonet.StatusInvalidArgument)
		return
	}
	results[0] = uint64(statusForError(session.session.GoAway(ErrorCode(uint32(params[1])), debug)))
}

func (manager *abiManager) streamPriorityUpdate(host wago.HostModule, params, results []uint64) {
	if len(params) != 4 || len(results) != 1 {
		return
	}
	session, memory, status := manager.lookup(host, uint32(params[0]))
	if status == wagonet.StatusOK {
		session.mu.Lock()
		defer session.mu.Unlock()
	}
	if status != wagonet.StatusOK {
		results[0] = uint64(status)
		return
	}
	value, ok := abiMemory(memory, uint32(params[2]), uint32(params[3]))
	if !ok {
		results[0] = uint64(wagonet.StatusInvalidArgument)
		return
	}
	results[0] = uint64(statusForError(session.session.PriorityUpdate(uint32(params[1]), value)))
}

func (manager *abiManager) streamReset(host wago.HostModule, params, results []uint64) {
	if len(params) != 3 || len(results) != 1 {
		return
	}
	session, _, status := manager.lookup(host, uint32(params[0]))
	if status == wagonet.StatusOK {
		session.mu.Lock()
		defer session.mu.Unlock()
	}
	if status == wagonet.StatusOK {
		status = statusForError(session.session.ResetStream(uint32(params[1]), ErrorCode(uint32(params[2]))))
	}
	results[0] = uint64(status)
}

func (manager *abiManager) eventNext(host wago.HostModule, params, results []uint64) {
	if len(params) != 2 || len(results) != 1 {
		return
	}
	session, memory, status := manager.lookup(host, uint32(params[0]))
	if status == wagonet.StatusOK {
		session.mu.Lock()
		defer session.mu.Unlock()
	}
	if status != wagonet.StatusOK {
		results[0] = uint64(status)
		return
	}
	dst, ok := abiMemory(memory, uint32(params[1]), ABIEventV1Size)
	if !ok {
		results[0] = uint64(wagonet.StatusInvalidArgument)
		return
	}
	event, exists := session.session.NextEvent()
	if !exists {
		results[0] = uint64(wagonet.StatusAgain)
		return
	}
	session.current = event
	session.hasCurrent = true
	clear(dst)
	binary.LittleEndian.PutUint32(dst[0:4], uint32(event.Type))
	binary.LittleEndian.PutUint32(dst[4:8], event.StreamID)
	flags := uint32(0)
	if event.EndStream {
		flags |= ABIFlagEndStream
	}
	if event.Trailer {
		flags |= 2
	}
	binary.LittleEndian.PutUint32(dst[8:12], flags)
	binary.LittleEndian.PutUint32(dst[12:16], uint32(event.ErrorCode))
	value := event.LastStreamID
	if event.Type == EventWindowUpdate {
		value = event.WindowIncrement
	}
	binary.LittleEndian.PutUint32(dst[16:20], value)
	binary.LittleEndian.PutUint32(dst[20:24], abiEventDataLength(event))
	binary.LittleEndian.PutUint32(dst[24:28], uint32(len(event.Headers)))
	binary.LittleEndian.PutUint32(dst[28:32], uint32(len(event.Settings)))
	results[0] = uint64(wagonet.StatusOK)
}

func (manager *abiManager) eventData(host wago.HostModule, params, results []uint64) {
	if len(params) != 4 || len(results) != 1 {
		return
	}
	session, memory, status := manager.lookup(host, uint32(params[0]))
	if status == wagonet.StatusOK {
		session.mu.Lock()
		defer session.mu.Unlock()
	}
	if status != wagonet.StatusOK || !session.hasCurrent {
		if status == wagonet.StatusOK {
			status = wagonet.StatusInvalidState
		}
		results[0] = uint64(status)
		return
	}
	dst, ok := abiMemory(memory, uint32(params[1]), uint32(params[2]))
	written, writtenOK := abiMemory(memory, uint32(params[3]), 4)
	if !ok || !writtenOK {
		results[0] = uint64(wagonet.StatusInvalidArgument)
		return
	}
	data := session.current.Data
	if session.current.Type == EventPing || session.current.Type == EventPingAck {
		data = session.current.Ping[:]
	}
	n := copy(dst, data)
	binary.LittleEndian.PutUint32(written, uint32(n))
	results[0] = uint64(wagonet.StatusOK)
}

func (manager *abiManager) eventHeader(host wago.HostModule, params, results []uint64) {
	if len(params) != 8 || len(results) != 1 {
		return
	}
	session, memory, status := manager.lookup(host, uint32(params[0]))
	if status == wagonet.StatusOK {
		session.mu.Lock()
		defer session.mu.Unlock()
	}
	index := uint32(params[1])
	if status != wagonet.StatusOK || !session.hasCurrent || index >= uint32(len(session.current.Headers)) {
		if status == wagonet.StatusOK {
			status = wagonet.StatusInvalidArgument
		}
		results[0] = uint64(status)
		return
	}
	field := session.current.Headers[index]
	name, nameOK := abiMemory(memory, uint32(params[2]), uint32(params[3]))
	nameLen, nameLenOK := abiMemory(memory, uint32(params[4]), 4)
	value, valueOK := abiMemory(memory, uint32(params[5]), uint32(params[6]))
	valueLen, valueLenOK := abiMemory(memory, uint32(params[7]), 4)
	if !nameOK || !nameLenOK || !valueOK || !valueLenOK {
		results[0] = uint64(wagonet.StatusInvalidArgument)
		return
	}
	copy(name, field.Name)
	copy(value, field.Value)
	binary.LittleEndian.PutUint32(nameLen, uint32(len(field.Name)))
	binary.LittleEndian.PutUint32(valueLen, uint32(len(field.Value)))
	if len(name) < len(field.Name) || len(value) < len(field.Value) {
		results[0] = uint64(wagonet.StatusMessageTooLarge)
		return
	}
	results[0] = uint64(wagonet.StatusOK)
}

func (manager *abiManager) eventSetting(host wago.HostModule, params, results []uint64) {
	if len(params) != 3 || len(results) != 1 {
		return
	}
	session, memory, status := manager.lookup(host, uint32(params[0]))
	if status == wagonet.StatusOK {
		session.mu.Lock()
		defer session.mu.Unlock()
	}
	index := uint32(params[1])
	if status != wagonet.StatusOK || !session.hasCurrent || index >= uint32(len(session.current.Settings)) {
		if status == wagonet.StatusOK {
			status = wagonet.StatusInvalidArgument
		}
		results[0] = uint64(status)
		return
	}
	dst, ok := abiMemory(memory, uint32(params[2]), ABISettingV1Size)
	if !ok {
		results[0] = uint64(wagonet.StatusInvalidArgument)
		return
	}
	setting := session.current.Settings[index]
	binary.LittleEndian.PutUint32(dst[0:4], uint32(setting.ID))
	binary.LittleEndian.PutUint32(dst[4:8], setting.Value)
	results[0] = uint64(wagonet.StatusOK)
}

func (manager *abiManager) lookup(host wago.HostModule, handle uint32) (*abiSession, []byte, wagonet.Status) {
	state, memory, status := manager.caller(host)
	if status != wagonet.StatusOK {
		return nil, nil, status
	}
	manager.mu.Lock()
	session := state.sessions[handle]
	manager.mu.Unlock()
	if session == nil {
		return nil, memory, wagonet.StatusBadHandle
	}
	return session, memory, wagonet.StatusOK
}

func abiMemory(memory []byte, pointer, length uint32) ([]byte, bool) {
	end := uint64(pointer) + uint64(length)
	if end > uint64(len(memory)) {
		return nil, false
	}
	return memory[pointer:uint32(end):uint32(end)], true
}

func readABIHeaders(memory []byte, pointer, count uint32, limits HeaderLimits) ([]HeaderField, wagonet.Status) {
	limits = limits.normalized()
	if count > limits.MaxHeaders || uint64(count)*uint64(ABIHeaderV1Size) > math.MaxUint32 {
		return nil, wagonet.StatusResourceLimit
	}
	encoded, ok := abiMemory(memory, pointer, count*ABIHeaderV1Size)
	if !ok {
		return nil, wagonet.StatusInvalidArgument
	}
	headers := make([]HeaderField, count)
	var listBytes uint64
	for index := uint32(0); index < count; index++ {
		entry := encoded[index*ABIHeaderV1Size : (index+1)*ABIHeaderV1Size]
		name, nameOK := abiMemory(memory, binary.LittleEndian.Uint32(entry[0:4]), binary.LittleEndian.Uint32(entry[4:8]))
		value, valueOK := abiMemory(memory, binary.LittleEndian.Uint32(entry[8:12]), binary.LittleEndian.Uint32(entry[12:16]))
		if !nameOK || !valueOK || len(name) > int(limits.MaxFieldBytes) || len(value) > int(limits.MaxFieldBytes) {
			return nil, wagonet.StatusInvalidArgument
		}
		listBytes += uint64(len(name)) + uint64(len(value)) + 32
		if listBytes > limits.MaxHeaderListBytes {
			return nil, wagonet.StatusResourceLimit
		}
		headers[index] = HeaderField{Name: string(name), Value: string(value), Sensitive: binary.LittleEndian.Uint32(entry[16:20])&ABIFlagSensitive != 0}
	}
	return headers, wagonet.StatusOK
}

func abiEventDataLength(event Event) uint32 {
	if event.Type == EventPing || event.Type == EventPingAck {
		return 8
	}
	return clampUint64To32(uint64(len(event.Data)))
}

func statusForError(err error) wagonet.Status {
	if err == nil {
		return wagonet.StatusOK
	}
	switch {
	case errors.Is(err, ErrWouldBlock):
		return wagonet.StatusAgain
	case errors.Is(err, ErrStreamNotFound):
		return wagonet.StatusBadHandle
	case errors.Is(err, ErrInvalidRole), errors.Is(err, ErrInvalidHeaders):
		return wagonet.StatusInvalidArgument
	case errors.Is(err, ErrStreamState), errors.Is(err, ErrSessionClosed), errors.Is(err, ErrSessionFailed):
		return wagonet.StatusInvalidState
	case errors.Is(err, ErrStreamLimit), errors.Is(err, ErrOutputLimit), errors.Is(err, ErrEventLimit), errors.Is(err, ErrStreamIDExhausted):
		return wagonet.StatusResourceLimit
	default:
		return wagonet.StatusIO
	}
}

func abiVersion(_ wago.HostModule, params, results []uint64) {
	if len(params) == 0 && len(results) == 1 {
		results[0] = uint64(ABIVersion1)
	}
}

func abiFeatureFlags(_ wago.HostModule, params, results []uint64) {
	if len(params) == 0 && len(results) == 1 {
		results[0] = FeatureAll
	}
}
