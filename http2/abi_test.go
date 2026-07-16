package http2

import (
	"encoding/binary"
	"errors"
	"sync"
	"testing"

	wagonet "github.com/wago-org/net"
	wago "github.com/wago-org/wago"
)

type abiTestHost struct {
	instance *wago.Instance
	memory   []byte
}

func (host *abiTestHost) Memory() []byte           { return host.memory }
func (host *abiTestHost) Instance() *wago.Instance { return host.instance }

func TestABISessionRoundTripAndLifecycle(t *testing.T) {
	manager := newABIManager(registerConfig{maxSessions: 2})
	clientHost := &abiTestHost{instance: &wago.Instance{}, memory: make([]byte, 64<<10)}
	serverHost := &abiTestHost{instance: &wago.Instance{}, memory: make([]byte, 64<<10)}
	clientHandle := abiOpen(t, manager, clientHost, RoleClient)
	serverHandle := abiOpen(t, manager, serverHost, RoleServer)

	clientWire := abiDrain(t, manager, clientHost, clientHandle)
	serverWire := abiDrain(t, manager, serverHost, serverHandle)
	abiFeed(t, manager, serverHost, serverHandle, clientWire)
	abiFeed(t, manager, clientHost, clientHandle, serverWire)
	abiFeed(t, manager, clientHost, clientHandle, abiDrain(t, manager, serverHost, serverHandle))
	abiFeed(t, manager, serverHost, serverHandle, abiDrain(t, manager, clientHost, clientHandle))

	headers := []HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "example.test"}, {Name: ":path", Value: "/"}}
	headerPtr := uint32(1024)
	writeABIHeaders(t, clientHost.memory, headerPtr, headers)
	results := []uint64{99}
	manager.streamOpen(clientHost, []uint64{uint64(clientHandle), uint64(headerPtr), uint64(len(headers)), uint64(ABIFlagEndStream), 512}, results)
	if results[0] != uint64(wagonet.StatusOK) {
		t.Fatalf("stream_open status=%d", results[0])
	}
	streamID := binary.LittleEndian.Uint32(clientHost.memory[512:516])
	if streamID != 1 {
		t.Fatalf("stream ID=%d", streamID)
	}
	abiFeed(t, manager, serverHost, serverHandle, abiDrain(t, manager, clientHost, clientHandle))

	for {
		results[0] = 99
		manager.eventNext(serverHost, []uint64{uint64(serverHandle), 600}, results)
		if results[0] == uint64(wagonet.StatusAgain) {
			break
		}
		if results[0] != uint64(wagonet.StatusOK) {
			t.Fatalf("event_next=%d", results[0])
		}
		typ := EventType(binary.LittleEndian.Uint32(serverHost.memory[600:604]))
		if typ == EventHeaders {
			count := binary.LittleEndian.Uint32(serverHost.memory[624:628])
			if count != 4 {
				t.Fatalf("header count=%d", count)
			}
			manager.eventHeader(serverHost, []uint64{uint64(serverHandle), 0, 700, 32, 740, 800, 32, 840}, results)
			if results[0] != uint64(wagonet.StatusOK) {
				t.Fatalf("event_header=%d", results[0])
			}
			if got := string(serverHost.memory[700 : 700+binary.LittleEndian.Uint32(serverHost.memory[740:744])]); got != ":method" {
				t.Fatalf("header=%q", got)
			}
		}
	}

	manager.closeInstance(clientHost.instance)
	results[0] = 99
	manager.sessionOutput(clientHost, []uint64{uint64(clientHandle), 0, 1, 8}, results)
	if results[0] != uint64(wagonet.StatusBadHandle) {
		t.Fatalf("closed instance status=%d", results[0])
	}
}

func TestABIControlAndStreamOperations(t *testing.T) {
	config := registerConfig{maxSessions: 4, session: SessionLimits{EnablePush: true, EnableExtendedConnect: true, MaxQueuedOutputBytes: 1 << 20}}
	WithMaxSessions(4).applyHTTP2(&config)
	WithSessionLimits(config.session).applyHTTP2(&config)
	manager := newABIManager(config)
	client := &abiTestHost{instance: &wago.Instance{}, memory: make([]byte, 64<<10)}
	server := &abiTestHost{instance: &wago.Instance{}, memory: make([]byte, 64<<10)}
	clientHandle := abiOpen(t, manager, client, RoleClient)
	serverHandle := abiOpen(t, manager, server, RoleServer)
	abiExchange(t, manager, client, clientHandle, server, serverHandle)

	requestHeaders := []HeaderField{{Name: ":method", Value: "POST"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/"}}
	writeABIHeaders(t, client.memory, 1024, requestHeaders)
	result := []uint64{99}
	manager.streamOpen(client, []uint64{uint64(clientHandle), 1024, uint64(len(requestHeaders)), 0, 512}, result)
	if result[0] != uint64(wagonet.StatusOK) {
		t.Fatalf("open=%d", result[0])
	}
	streamID := binary.LittleEndian.Uint32(client.memory[512:516])
	copy(client.memory[2000:], "abc")
	manager.streamData(client, []uint64{uint64(clientHandle), uint64(streamID), 2000, 3, uint64(ABIFlagEndStream), 520}, result)
	if result[0] != uint64(wagonet.StatusOK) || binary.LittleEndian.Uint32(client.memory[520:524]) != 3 {
		t.Fatalf("data=%d", result[0])
	}
	copy(client.memory[2100:], "u=1")
	manager.streamPriorityUpdate(client, []uint64{uint64(clientHandle), uint64(streamID), 2100, 3}, result)
	if result[0] != uint64(wagonet.StatusOK) {
		t.Fatalf("priority=%d", result[0])
	}
	abiFeed(t, manager, server, serverHandle, abiDrain(t, manager, client, clientHandle))

	pushHeaders := []HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/asset"}}
	writeABIHeaders(t, server.memory, 1024, pushHeaders)
	manager.streamPush(server, []uint64{uint64(serverHandle), uint64(streamID), 1024, uint64(len(pushHeaders)), 540}, result)
	if result[0] != uint64(wagonet.StatusOK) {
		t.Fatalf("push=%d", result[0])
	}
	promisedID := binary.LittleEndian.Uint32(server.memory[540:544])
	writeABIHeaders(t, server.memory, 3000, []HeaderField{{Name: ":status", Value: "204"}})
	manager.streamHeaders(server, []uint64{uint64(serverHandle), uint64(promisedID), 3000, 1, uint64(ABIFlagEndStream)}, result)
	if result[0] != uint64(wagonet.StatusOK) {
		t.Fatalf("push headers=%d", result[0])
	}
	writeABIHeaders(t, server.memory, 3100, []HeaderField{{Name: ":status", Value: "200"}})
	manager.streamHeaders(server, []uint64{uint64(serverHandle), uint64(streamID), 3100, 1, uint64(ABIFlagEndStream)}, result)
	if result[0] != uint64(wagonet.StatusOK) {
		t.Fatalf("headers=%d", result[0])
	}

	binary.LittleEndian.PutUint32(server.memory[3500:3504], uint32(SettingMaxConcurrentStreams))
	binary.LittleEndian.PutUint32(server.memory[3504:3508], 3)
	manager.sessionSettings(server, []uint64{uint64(serverHandle), 3500, 1}, result)
	if result[0] != uint64(wagonet.StatusOK) {
		t.Fatalf("settings=%d", result[0])
	}
	copy(server.memory[3600:], "12345678")
	manager.sessionPing(server, []uint64{uint64(serverHandle), 3600}, result)
	if result[0] != uint64(wagonet.StatusOK) {
		t.Fatalf("ping=%d", result[0])
	}
	manager.sessionGoAway(server, []uint64{uint64(serverHandle), uint64(ErrCodeNo), 0, 0}, result)
	if result[0] != uint64(wagonet.StatusOK) {
		t.Fatalf("goaway=%d", result[0])
	}
	abiFeed(t, manager, client, clientHandle, abiDrain(t, manager, server, serverHandle))

	for {
		manager.eventNext(client, []uint64{uint64(clientHandle), 600}, result)
		if result[0] == uint64(wagonet.StatusAgain) {
			break
		}
		if result[0] != uint64(wagonet.StatusOK) {
			t.Fatalf("event=%d", result[0])
		}
		typ := EventType(binary.LittleEndian.Uint32(client.memory[600:604]))
		if typ == EventSettings {
			manager.eventSetting(client, []uint64{uint64(clientHandle), 0, 700}, result)
			if result[0] != uint64(wagonet.StatusOK) {
				t.Fatalf("event setting=%d", result[0])
			}
		}
		if typ == EventPing {
			manager.eventData(client, []uint64{uint64(clientHandle), 720, 8, 740}, result)
			if result[0] != uint64(wagonet.StatusOK) || string(client.memory[720:728]) != "12345678" {
				t.Fatalf("event data=%d", result[0])
			}
		}
	}
	manager.streamReset(client, []uint64{uint64(clientHandle), uint64(streamID), uint64(ErrCodeCancel)}, result)
	manager.closeAll()
	var version [1]uint64
	abiVersion(nil, nil, version[:])
	if version[0] != uint64(ABIVersion1) {
		t.Fatalf("version=%d", version[0])
	}
	abiFeatureFlags(nil, nil, version[:])
	if version[0] != FeatureAll {
		t.Fatalf("features=%d", version[0])
	}
}

func TestABIConcurrentSerializedAccess(t *testing.T) {
	manager := newABIManager(registerConfig{session: SessionLimits{MaxQueuedOutputBytes: 1 << 20}})
	host := &abiTestHost{instance: &wago.Instance{}, memory: make([]byte, 64)}
	handle := abiOpen(t, manager, host, RoleClient)
	copy(host.memory[8:], "12345678")
	var wait sync.WaitGroup
	for index := 0; index < 32; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result := []uint64{99}
			manager.sessionPing(host, []uint64{uint64(handle), 8}, result)
		}()
	}
	wait.Wait()
}

func TestABIErrorAndMemoryCoverage(t *testing.T) {
	manager := newABIManager(registerConfig{session: SessionLimits{MaxQueuedOutputBytes: 256, Header: HeaderLimits{MaxHeaders: 2, MaxFieldBytes: 8, MaxHeaderListBytes: 48}}})
	host := &abiTestHost{instance: &wago.Instance{}, memory: make([]byte, 256)}
	handle := abiOpen(t, manager, host, RoleClient)
	result := []uint64{99}

	manager.streamOpen(host, []uint64{uint64(handle), 250, 1, 0, 0}, result)
	if result[0] == uint64(wagonet.StatusOK) {
		t.Fatal("bad stream_open memory")
	}
	manager.streamHeaders(host, []uint64{uint64(handle), 1, 250, 1, 0}, result)
	if result[0] == uint64(wagonet.StatusOK) {
		t.Fatal("bad stream_headers memory")
	}
	manager.streamData(host, []uint64{uint64(handle), 1, 255, 2, 0, 0}, result)
	if result[0] != uint64(wagonet.StatusInvalidArgument) {
		t.Fatalf("stream_data=%d", result[0])
	}
	manager.streamPush(host, []uint64{uint64(handle), 1, 250, 1, 0}, result)
	if result[0] == uint64(wagonet.StatusOK) {
		t.Fatal("bad stream_push memory")
	}
	manager.streamPriorityUpdate(host, []uint64{uint64(handle), 1, 255, 2}, result)
	if result[0] != uint64(wagonet.StatusInvalidArgument) {
		t.Fatalf("priority=%d", result[0])
	}
	manager.sessionSettings(host, []uint64{uint64(handle), 255, 1}, result)
	if result[0] != uint64(wagonet.StatusInvalidArgument) {
		t.Fatalf("settings=%d", result[0])
	}
	manager.sessionPing(host, []uint64{uint64(handle), 252}, result)
	if result[0] != uint64(wagonet.StatusInvalidArgument) {
		t.Fatalf("ping=%d", result[0])
	}
	manager.sessionGoAway(host, []uint64{uint64(handle), 0, 255, 2}, result)
	if result[0] != uint64(wagonet.StatusInvalidArgument) {
		t.Fatalf("goaway=%d", result[0])
	}
	manager.eventNext(host, []uint64{uint64(handle), 250}, result)
	if result[0] != uint64(wagonet.StatusInvalidArgument) {
		t.Fatalf("event_next=%d", result[0])
	}
	manager.eventData(host, []uint64{uint64(handle), 0, 1, 4}, result)
	if result[0] != uint64(wagonet.StatusInvalidState) {
		t.Fatalf("event_data state=%d", result[0])
	}
	manager.eventHeader(host, []uint64{uint64(handle), 0, 0, 1, 4, 8, 1, 12}, result)
	if result[0] != uint64(wagonet.StatusInvalidArgument) {
		t.Fatalf("event_header index=%d", result[0])
	}
	manager.eventSetting(host, []uint64{uint64(handle), 0, 0}, result)
	if result[0] != uint64(wagonet.StatusInvalidArgument) {
		t.Fatalf("event_setting index=%d", result[0])
	}

	manager.mu.Lock()
	abi := manager.instances[host.instance].sessions[handle]
	manager.mu.Unlock()
	abi.mu.Lock()
	abi.current = Event{Type: EventHeaders, Headers: []HeaderField{{Name: "long-name", Value: "long-value"}}, Settings: []Setting{{ID: SettingHeaderTableSize, Value: 1}}, Data: []byte("data")}
	abi.hasCurrent = true
	abi.mu.Unlock()
	manager.eventData(host, []uint64{uint64(handle), 255, 2, 0}, result)
	if result[0] != uint64(wagonet.StatusInvalidArgument) {
		t.Fatalf("event_data memory=%d", result[0])
	}
	manager.eventHeader(host, []uint64{uint64(handle), 0, 0, 2, 4, 8, 2, 12}, result)
	if result[0] != uint64(wagonet.StatusMessageTooLarge) {
		t.Fatalf("event_header small=%d", result[0])
	}
	manager.eventSetting(host, []uint64{uint64(handle), 0, 252}, result)
	if result[0] != uint64(wagonet.StatusInvalidArgument) {
		t.Fatalf("event_setting memory=%d", result[0])
	}

	for _, err := range []error{nil, ErrWouldBlock, ErrStreamNotFound, ErrInvalidRole, ErrInvalidHeaders, ErrStreamState, ErrSessionClosed, ErrSessionFailed, ErrStreamLimit, ErrOutputLimit, ErrEventLimit, ErrStreamIDExhausted, errors.New("io")} {
		_ = statusForError(err)
	}
	if _, status := readABIHeaders(host.memory, 0, 3, HeaderLimits{MaxHeaders: 2}); status != wagonet.StatusResourceLimit {
		t.Fatalf("header count=%d", status)
	}
	if _, status := readABIHeaders(host.memory, 255, 1, HeaderLimits{}); status != wagonet.StatusInvalidArgument {
		t.Fatalf("header pointer=%d", status)
	}
	binary.LittleEndian.PutUint32(host.memory[0:4], 32)
	binary.LittleEndian.PutUint32(host.memory[4:8], 9)
	if _, status := readABIHeaders(host.memory, 0, 1, HeaderLimits{MaxFieldBytes: 8}); status != wagonet.StatusInvalidArgument {
		t.Fatalf("field size=%d", status)
	}
}

func TestABIRejectsBoundsHandlesAndQuotas(t *testing.T) {
	manager := newABIManager(registerConfig{maxSessions: 1})
	host := &abiTestHost{instance: &wago.Instance{}, memory: make([]byte, 64)}
	handle := abiOpen(t, manager, host, RoleClient)
	results := []uint64{99}
	manager.sessionOpen(host, []uint64{uint64(RoleClient), 4}, results)
	if results[0] != uint64(wagonet.StatusResourceLimit) {
		t.Fatalf("quota=%d", results[0])
	}
	manager.sessionFeed(host, []uint64{uint64(handle), 63, 2, 0}, results)
	if results[0] != uint64(wagonet.StatusInvalidArgument) {
		t.Fatalf("bounds=%d", results[0])
	}
	manager.sessionClose(host, []uint64{999}, results)
	if results[0] != uint64(wagonet.StatusBadHandle) {
		t.Fatalf("handle=%d", results[0])
	}
	manager.sessionClose(host, []uint64{uint64(handle)}, results)
	if results[0] != uint64(wagonet.StatusOK) {
		t.Fatalf("close=%d", results[0])
	}

	unidentified := &abiMemoryOnly{memory: make([]byte, 16)}
	manager.sessionOpen(unidentified, []uint64{uint64(RoleClient), 0}, results)
	if results[0] != uint64(wagonet.StatusInvalidState) {
		t.Fatalf("identity=%d", results[0])
	}
}

type abiMemoryOnly struct{ memory []byte }

func (host *abiMemoryOnly) Memory() []byte { return host.memory }

func abiOpen(t *testing.T, manager *abiManager, host *abiTestHost, role Role) uint32 {
	t.Helper()
	results := []uint64{99}
	manager.sessionOpen(host, []uint64{uint64(role), 0}, results)
	if results[0] != uint64(wagonet.StatusOK) {
		t.Fatalf("session_open=%d", results[0])
	}
	return binary.LittleEndian.Uint32(host.memory[:4])
}

func abiDrain(t *testing.T, manager *abiManager, host *abiTestHost, handle uint32) []byte {
	t.Helper()
	var wire []byte
	for {
		results := []uint64{99}
		manager.sessionOutput(host, []uint64{uint64(handle), 4096, 4096, 8}, results)
		if results[0] == uint64(wagonet.StatusAgain) {
			return wire
		}
		if results[0] != uint64(wagonet.StatusOK) {
			t.Fatalf("session_output=%d", results[0])
		}
		n := binary.LittleEndian.Uint32(host.memory[8:12])
		wire = append(wire, host.memory[4096:4096+n]...)
	}
}

func abiExchange(t *testing.T, manager *abiManager, left *abiTestHost, leftHandle uint32, right *abiTestHost, rightHandle uint32) {
	t.Helper()
	for round := 0; round < 4; round++ {
		abiFeed(t, manager, right, rightHandle, abiDrain(t, manager, left, leftHandle))
		abiFeed(t, manager, left, leftHandle, abiDrain(t, manager, right, rightHandle))
	}
}

func abiFeed(t *testing.T, manager *abiManager, host *abiTestHost, handle uint32, wire []byte) {
	t.Helper()
	if len(wire) == 0 {
		return
	}
	copy(host.memory[8192:], wire)
	results := []uint64{99}
	manager.sessionFeed(host, []uint64{uint64(handle), 8192, uint64(len(wire)), 12}, results)
	if results[0] != uint64(wagonet.StatusOK) {
		t.Fatalf("session_feed=%d", results[0])
	}
	if n := binary.LittleEndian.Uint32(host.memory[12:16]); n != uint32(len(wire)) {
		t.Fatalf("feed=%d/%d", n, len(wire))
	}
}

func writeABIHeaders(t *testing.T, memory []byte, pointer uint32, fields []HeaderField) {
	t.Helper()
	data := uint32(4096)
	for index, field := range fields {
		entry := memory[pointer+uint32(index)*ABIHeaderV1Size:]
		binary.LittleEndian.PutUint32(entry[0:4], data)
		binary.LittleEndian.PutUint32(entry[4:8], uint32(len(field.Name)))
		copy(memory[data:], field.Name)
		data += uint32(len(field.Name))
		binary.LittleEndian.PutUint32(entry[8:12], data)
		binary.LittleEndian.PutUint32(entry[12:16], uint32(len(field.Value)))
		copy(memory[data:], field.Value)
		data += uint32(len(field.Value))
		if field.Sensitive {
			binary.LittleEndian.PutUint32(entry[16:20], ABIFlagSensitive)
		}
	}
}
