package wire_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/phenomenon0/MacHoma/internal/wire"
)

type cLayout struct {
	Size    int            `json:"size"`
	Offsets map[string]int `json:"offsets"`
}

type cFixtures struct {
	Revision  string             `json:"revision"`
	Layouts   map[string]cLayout `json:"layouts"`
	Constants map[string]int     `json:"constants"`
	Packets   map[string]string  `json:"packets"`
}

func loadFixtures(t testing.TB) cFixtures {
	t.Helper()
	b, err := os.ReadFile("testdata/upstream-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures cFixtures
	if err := json.Unmarshal(b, &fixtures); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func fixtureCommon(typ wire.Type) wire.Common {
	h := wire.NewCommon(typ, 0x1234, 0xabcd, 0x0102030405060708)
	h.Sequence = 0x10203040
	h.Ack = [3]byte{0x11, 0x22, 0x33}
	h.Flags, h.Window, h.Checksum, h.Urgent = 0x42, 0x5678, 0x9abc, wire.HijackUrgent
	return h
}

func fixturePackets() map[string]wire.Packet {
	cutoffs := wire.Cutoffs{Common: fixtureCommon(wire.CutoffsType), CutoffVersion: 0x5678}
	for i := range cutoffs.UnscheduledCutoffs {
		cutoffs.UnscheduledCutoffs[i] = 0x12340000 + uint32(i)
	}
	ack := wire.Ack{Common: fixtureCommon(wire.AckType), NumAcks: 2}
	for i := range ack.Acks {
		ack.Acks[i] = wire.AckRecord{ClientID: 0x1020304050607000 + uint64(i*2), ServerPort: uint16(0x4000 + i)}
	}
	return map[string]wire.Packet{
		"DATA": wire.Data{Common: fixtureCommon(wire.DataType), MessageLength: 1024, Incoming: 512,
			Ack:           wire.AckRecord{ClientID: 0x1020304050607080, ServerPort: 0x2233},
			CutoffVersion: 0x4567, Retransmit: 1, Padding: [3]byte{0x12, 0x34, 0x56}, SegmentOffset: 17, Payload: []byte("payload")},
		"GRANT":       wire.Grant{Common: fixtureCommon(wire.GrantType), Offset: 0x11223344, Priority: 7},
		"RESEND":      wire.Resend{Common: fixtureCommon(wire.ResendType), Offset: 0x23456789, Length: wire.ResendAll, Priority: 6},
		"RPC_UNKNOWN": wire.RPCUnknown{Common: fixtureCommon(wire.RPCUnknownType)},
		"BUSY":        wire.Busy{Common: fixtureCommon(wire.BusyType)},
		"CUTOFFS":     cutoffs,
		"FREEZE":      wire.Freeze{Common: fixtureCommon(wire.FreezeType)},
		"NEED_ACK":    wire.NeedAck{Common: fixtureCommon(wire.NeedAckType)},
		"ACK":         ack,
	}
}

// The oracle is compiled C using the actual upstream structs, not bytes emitted
// by this Go encoder. This catches mutually consistent encoder/decoder mistakes.
func TestPacketsMatchUpstreamCBytesAndFields(t *testing.T) {
	fixtures := loadFixtures(t)
	packets := fixturePackets()
	if len(fixtures.Packets) != len(packets) {
		t.Fatalf("missing fixture packet: C=%d Go=%d", len(fixtures.Packets), len(packets))
	}
	for name, packet := range packets {
		t.Run(name, func(t *testing.T) {
			golden, err := hex.DecodeString(fixtures.Packets[name])
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := wire.Marshal(packet)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(encoded, golden) {
				t.Fatalf("Go bytes differ from upstream C\n Go: %x\n  C: %x", encoded, golden)
			}
			decoded, err := wire.Parse(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, packet) {
				t.Fatalf("decoded fields differ:\ngot  %#v\nwant %#v", decoded, packet)
			}
			again, err := wire.Marshal(decoded)
			if err != nil || !bytes.Equal(again, golden) {
				t.Fatalf("round trip changed C packet: %x err=%v", again, err)
			}
		})
	}
}

func TestUpstreamCLayoutsAndConstants(t *testing.T) {
	fixtures := loadFixtures(t)
	if fixtures.Revision != "d8914b8a57aa48c19a2c8130484961585bcbe53c" {
		t.Fatalf("unexpected C source revision %q", fixtures.Revision)
	}
	wantLayouts := map[string]cLayout{
		"homa_common_hdr":      {wire.CommonSize, map[string]int{"sport": 0, "dport": 2, "sequence": 4, "ack": 8, "type": 11, "doff": 12, "flags": 13, "window": 14, "checksum": 16, "urgent": 18, "sender_id": 20}},
		"homa_ack":             {wire.AckRecordSize, map[string]int{"client_id": 0, "server_port": 8}},
		"homa_seg_hdr":         {wire.SegmentSize, map[string]int{"offset": 0}},
		"homa_data_hdr":        {wire.DataSize, map[string]int{"common": 0, "message_length": 28, "incoming": 32, "ack": 36, "cutoff_version": 46, "retransmit": 48, "pad": 49, "seg": 52}},
		"homa_grant_hdr":       {wire.GrantSize, map[string]int{"common": 0, "offset": 28, "priority": 32}},
		"homa_resend_hdr":      {wire.ResendSize, map[string]int{"common": 0, "offset": 28, "length": 32, "priority": 36}},
		"homa_rpc_unknown_hdr": {wire.CommonSize, map[string]int{"common": 0}},
		"homa_busy_hdr":        {wire.CommonSize, map[string]int{"common": 0}},
		"homa_cutoffs_hdr":     {wire.CutoffsSize, map[string]int{"common": 0, "unsched_cutoffs": 28, "cutoff_version": 60}},
		"homa_freeze_hdr":      {wire.CommonSize, map[string]int{"common": 0}},
		"homa_need_ack_hdr":    {wire.CommonSize, map[string]int{"common": 0}},
		"homa_ack_hdr":         {wire.AckSize, map[string]int{"common": 0, "num_acks": 28, "acks": 30}},
	}
	if !reflect.DeepEqual(fixtures.Layouts, wantLayouts) {
		t.Fatalf("Go header layout assumptions differ from upstream C:\ngot %#v\nwant %#v", wantLayouts, fixtures.Layouts)
	}
	wantConstants := map[string]int{
		"DATA": int(wire.DataType), "GRANT": int(wire.GrantType), "RESEND": int(wire.ResendType),
		"RPC_UNKNOWN": int(wire.RPCUnknownType), "BUSY": int(wire.BusyType), "CUTOFFS": int(wire.CutoffsType),
		"FREEZE": int(wire.FreezeType), "NEED_ACK": int(wire.NeedAckType), "ACK": int(wire.AckType),
		"HOMA_MAX_PRIORITIES": wire.MaxPriorities, "HOMA_MAX_ACKS_PER_PKT": wire.MaxAcks,
		"HOMA_MIN_PKT_LENGTH": wire.MinPacketLength, "HOMA_MAX_HEADER": wire.MaxHeader, "HOMA_HIJACK_URGENT": wire.HijackUrgent,
	}
	if !reflect.DeepEqual(fixtures.Constants, wantConstants) {
		t.Fatalf("opcode/constant mismatch:\nGo %#v\nC %#v", wantConstants, fixtures.Constants)
	}
}

func TestPinnedUpstreamHeaderAndFreshCCompilerConformance(t *testing.T) {
	header, err := os.ReadFile("testdata/upstream/homa_wire.h")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(header)); got != "84ee73542f3825d53427a712908df92ff8eee84e515a56d792e29b44a4fe21fa" {
		t.Fatalf("pinned upstream header changed: %s", got)
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler unavailable; committed upstream-C golden tests still run")
	}
	binary := filepath.Join(t.TempDir(), "homa-wire-fixtures")
	cmd := exec.Command(cc, "-std=c11", "-Wall", "-Wextra", "-Werror", "-Itestdata/shim", "testdata/fixtures.c", "-o", binary)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile pinned upstream C header: %v\n%s", err, output)
	}
	fresh, err := exec.Command(binary).Output()
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("testdata/upstream-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fresh, golden) {
		t.Fatal("fresh C sizeof/offsetof/packet bytes differ from committed golden")
	}
}

func TestEveryHeaderRejectsEveryTruncation(t *testing.T) {
	for name, hexBytes := range loadFixtures(t).Packets {
		t.Run(name, func(t *testing.T) {
			packet, err := hex.DecodeString(hexBytes)
			if err != nil {
				t.Fatal(err)
			}
			headerLen, ok := wire.HeaderSize(wire.Type(packet[11]))
			if !ok {
				t.Fatal("known packet type has no header length")
			}
			for n := 0; n < headerLen; n++ {
				if _, err := wire.Parse(packet[:n]); !errors.Is(err, wire.ErrTruncated) {
					t.Fatalf("prefix length %d: %v", n, err)
				}
			}
			if _, err := wire.Parse(packet[:headerLen]); err != nil {
				t.Fatalf("complete fixed header rejected: %v", err)
			}
		})
	}
}

func TestAckAlwaysCarriesFiveFixedSlots(t *testing.T) {
	for count := uint16(0); count <= wire.MaxAcks; count++ {
		p := wire.Ack{Common: wire.NewCommon(wire.AckType, 1, 2, 4), NumAcks: count}
		for i := range p.Acks {
			p.Acks[i] = wire.AckRecord{ClientID: uint64(i + 8), ServerPort: uint16(i + 100)}
		}
		b, err := wire.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) != 80 {
			t.Fatalf("ACK count=%d header length=%d, want 80", count, len(b))
		}
		got, err := wire.Parse(b)
		if err != nil || !reflect.DeepEqual(got, p) {
			t.Fatalf("ACK count=%d slots lost: %#v %v", count, got, err)
		}
	}
	for _, count := range []uint16{6, 65535} {
		p := wire.Ack{Common: wire.NewCommon(wire.AckType, 1, 2, 4), NumAcks: count}
		if _, err := wire.Marshal(p); !errors.Is(err, wire.ErrAckCount) {
			t.Fatalf("marshal ACK count=%d: %v", count, err)
		}
		p.NumAcks = 0
		b, err := wire.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		b[28], b[29] = byte(count>>8), byte(count)
		if _, err := wire.Parse(b); !errors.Is(err, wire.ErrAckCount) {
			t.Fatalf("parse ACK count=%d: %v", count, err)
		}
	}
}

func TestCommonIDsDefaultsAndSentinelOffsets(t *testing.T) {
	for _, id := range []uint64{0, 1, 2, 3, ^uint64(0)} {
		h := wire.NewCommon(wire.DataType, 10, 11, id)
		if h.LocalID() != id^1 || h.DataOffset != 0xd0 {
			t.Fatalf("DATA common incorrect: %#v", h)
		}
	}
	h := wire.NewCommon(wire.BusyType, 10, 11, 18)
	if h.DataOffset != 0x50 {
		t.Fatalf("control doff=%x", h.DataOffset)
	}
	p := wire.Data{Common: wire.Common{Sequence: 99}, SegmentOffset: wire.SequenceOffset}
	if p.EffectiveOffset() != 99 {
		t.Fatal("GSO sentinel did not use sequence offset")
	}
	p.SegmentOffset = 15
	if p.EffectiveOffset() != 15 {
		t.Fatal("native offset overwritten by sequence")
	}
}

func TestDataBoundsNeverWrapAndEnforceCallerLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		p     wire.Data
		limit uint32
		valid bool
	}{
		{"last byte", wire.Data{MessageLength: 100, SegmentOffset: 99, Payload: []byte{1}}, 100, true},
		{"past end", wire.Data{MessageLength: 100, SegmentOffset: 99, Payload: []byte{1, 2}}, 100, false},
		{"overflow", wire.Data{MessageLength: 100, SegmentOffset: ^uint32(0) - 1, Payload: []byte{1, 2, 3}}, 100, false},
		{"zero length", wire.Data{}, 100, false},
		{"message cap", wire.Data{MessageLength: 101}, 100, false},
		{"GSO sentinel", wire.Data{Common: wire.Common{Sequence: 99}, MessageLength: 100, SegmentOffset: wire.SequenceOffset, Payload: []byte{1}}, 100, true},
		{"GSO overflow", wire.Data{Common: wire.Common{Sequence: ^uint32(0)}, MessageLength: 100, SegmentOffset: wire.SequenceOffset, Payload: []byte{1}}, 100, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.ValidateBounds(tc.limit)
			if tc.valid && err != nil {
				t.Fatal(err)
			}
			if !tc.valid && !errors.Is(err, wire.ErrDataBounds) {
				t.Fatalf("invalid segment accepted: %v", err)
			}
		})
	}
}

func TestUnknownAndMismatchedTypesAreRejected(t *testing.T) {
	for n := 0; n < 256; n++ {
		typ := wire.Type(n)
		_, valid := wire.HeaderSize(typ)
		if valid {
			continue
		}
		b := make([]byte, wire.MaxHeader)
		b[11] = byte(n)
		if _, err := wire.Parse(b); !errors.Is(err, wire.ErrType) {
			t.Fatalf("unknown type %x: %v", n, err)
		}
	}
	var nilData *wire.Data
	for _, p := range []wire.Packet{nil, nilData, wire.Data{Common: wire.NewCommon(wire.GrantType, 1, 2, 4)}, wire.CommonPacket{Common: wire.NewCommon(wire.DataType, 1, 2, 4)}} {
		if _, err := wire.Marshal(p); !errors.Is(err, wire.ErrType) {
			t.Fatalf("invalid packet %T: %v", p, err)
		}
	}
}

func TestControlPaddingAndCommonPacket(t *testing.T) {
	p := wire.CommonPacket{Common: wire.NewCommon(wire.NeedAckType, 123, 456, 20)}
	b, err := wire.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	padded := append(append([]byte(nil), b...), 0xff, 0xfe, 0xfd)
	got, err := wire.Parse(padded)
	if err != nil || got.Header() != p.Common {
		t.Fatalf("padded control rejected: %#v %v", got, err)
	}
	again, err := wire.Marshal(got)
	if err != nil || !bytes.Equal(again, b) {
		t.Fatalf("control padding became header data: %x %v", again, err)
	}
}

func FuzzParse(f *testing.F) {
	for _, value := range loadFixtures(f).Packets {
		b, err := hex.DecodeString(value)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		packet, err := wire.Parse(b)
		if err != nil {
			return
		}
		encoded, err := wire.Marshal(packet)
		if err != nil {
			t.Fatalf("parsed packet cannot encode: %v", err)
		}
		want := b
		if packet.Header().Type != wire.DataType {
			size, _ := wire.HeaderSize(packet.Header().Type)
			want = b[:size]
		}
		if !bytes.Equal(encoded, want) {
			t.Fatalf("accepted packet changed in round trip:\nin %x\nout %x", want, encoded)
		}
		if data, ok := packet.(wire.Data); ok {
			_ = data.ValidateBounds(wire.MaxMessageLength)
		}
	})
}
