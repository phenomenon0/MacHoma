// Package wire encodes Homa's native transport headers, without an IP header.
// Layouts match PlatformLab/HomaModule commit
// d8914b8a57aa48c19a2c8130484961585bcbe53c, homa_wire.h (full, unstripped build).
// This package does not implement TCP hijacking, IP checksums, or GSO framing.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
)

type Type uint8

const (
	DataType Type = 0x10 + iota
	GrantType
	ResendType
	RPCUnknownType
	BusyType
	CutoffsType
	FreezeType
	NeedAckType
	AckType
)

const (
	IPProtocol       = 146
	CommonSize       = 28
	AckRecordSize    = 10
	SegmentSize      = 4
	DataSize         = 56
	GrantSize        = 33
	ResendSize       = 37
	CutoffsSize      = 62
	AckSize          = 80
	MaxPriorities    = 8
	MaxAcks          = 5
	MinPacketLength  = 26
	MaxHeader        = 90
	MaxMessageLength = 1_000_000
	HijackUrgent     = 0xb97d
	// The DATA prefix replicated by TSO excludes the four-byte segment header.
	DataOffset     = uint8((DataSize - SegmentSize) / 4 << 4)
	ControlOffset  = uint8(5 << 4)
	SequenceOffset = uint32(0xffffffff)
	ResendAll      = uint32(0xffffffff)
)

var (
	ErrTruncated  = errors.New("homa wire: truncated packet")
	ErrType       = errors.New("homa wire: unknown or mismatched packet type")
	ErrAckCount   = errors.New("homa wire: more than five acknowledgment records")
	ErrDataBounds = errors.New("homa wire: segment exceeds message bounds")
)

// Common contains all bytes of homa_common_hdr, including fields unused by
// native Homa. Numeric fields use host byte order in Go and big endian on wire.
// DataOffset is the full raw byte (e.g. 0xd0), not a count of 32-bit words.
type Common struct {
	SourcePort, DestinationPort uint16
	Sequence                    uint32
	Ack                         [3]byte
	Type                        Type
	DataOffset, Flags           uint8
	Window, Checksum, Urgent    uint16
	SenderID                    uint64
}

func (h Common) Header() Common { return h }

// LocalID converts the sender's even-client/odd-server ID to the recipient ID.
func (h Common) LocalID() uint64 { return h.SenderID ^ 1 }

// NewCommon fills the native packet type, ports, ID and correct TCP-compatible
// data offset. Other common fields are zero. Marshal preserves all these fields.
func NewCommon(typ Type, source, destination uint16, senderID uint64) Common {
	offset := ControlOffset
	if typ == DataType {
		offset = DataOffset
	}
	return Common{SourcePort: source, DestinationPort: destination, Type: typ, DataOffset: offset, SenderID: senderID}
}

// Packet is implemented by the value forms of all packet structs below; their
// pointers are also accepted by Marshal. Header returns a copy.
type Packet interface{ Header() Common }

type AckRecord struct {
	ClientID   uint64
	ServerPort uint16
}

type Data struct {
	Common
	MessageLength, Incoming uint32
	Ack                     AckRecord
	CutoffVersion           uint16
	Retransmit              uint8
	Padding                 [3]byte
	SegmentOffset           uint32
	Payload                 []byte
}

// EffectiveOffset handles the TCP-GSO sentinel documented in homa_seg_hdr.
// Native protocol-146 transmission should normally use SegmentOffset directly.
func (p Data) EffectiveOffset() uint32 {
	if p.SegmentOffset == SequenceOffset {
		return p.Sequence
	}
	return p.SegmentOffset
}

// ValidateBounds applies message policy separately from lossless header decoding.
// Widened arithmetic prevents an overflowing offset from passing the check.
func (p Data) ValidateBounds(maxMessage uint32) error {
	if p.MessageLength == 0 || p.MessageLength > maxMessage ||
		uint64(p.EffectiveOffset())+uint64(len(p.Payload)) > uint64(p.MessageLength) {
		return ErrDataBounds
	}
	return nil
}

type Grant struct {
	Common
	Offset   uint32
	Priority uint8
}
type Resend struct {
	Common
	Offset, Length uint32
	Priority       uint8
}
type RPCUnknown struct{ Common }
type Busy struct{ Common }
type Cutoffs struct {
	Common
	UnscheduledCutoffs [MaxPriorities]uint32
	CutoffVersion      uint16
}
type Freeze struct{ Common }
type NeedAck struct{ Common }
type Ack struct {
	Common
	NumAcks uint16
	Acks    [MaxAcks]AckRecord
}

// CommonPacket permits constructing RPC_UNKNOWN, BUSY, FREEZE, or NEED_ACK
// without choosing a dedicated Go struct. Its Common.Type must be one of these.
type CommonPacket struct{ Common }

func HeaderSize(typ Type) (int, bool) {
	switch typ {
	case DataType:
		return DataSize, true
	case GrantType:
		return GrantSize, true
	case ResendType:
		return ResendSize, true
	case RPCUnknownType, BusyType, FreezeType, NeedAckType:
		return CommonSize, true
	case CutoffsType:
		return CutoffsSize, true
	case AckType:
		return AckSize, true
	default:
		return 0, false
	}
}

func parseCommon(b []byte) Common {
	return Common{
		SourcePort: binary.BigEndian.Uint16(b[0:2]), DestinationPort: binary.BigEndian.Uint16(b[2:4]),
		Sequence: binary.BigEndian.Uint32(b[4:8]), Ack: [3]byte{b[8], b[9], b[10]},
		Type: Type(b[11]), DataOffset: b[12], Flags: b[13],
		Window: binary.BigEndian.Uint16(b[14:16]), Checksum: binary.BigEndian.Uint16(b[16:18]),
		Urgent: binary.BigEndian.Uint16(b[18:20]), SenderID: binary.BigEndian.Uint64(b[20:28]),
	}
}

func parseAck(b []byte) AckRecord {
	return AckRecord{ClientID: binary.BigEndian.Uint64(b[0:8]), ServerPort: binary.BigEndian.Uint16(b[8:10])}
}

// Parse decodes one received transport datagram (not a pre-segmentation GSO
// buffer), returning a concrete VALUE: Data, Grant, Resend, RPCUnknown, Busy,
// Cutoffs, Freeze, NeedAck, or Ack. Data.Payload aliases b; copy it before reusing
// a receive buffer. Control packets may have trailing Ethernet/IP padding, which
// is ignored as in the Linux implementation. Full fixed headers are required,
// including all five ACK slots even when NumAcks is zero.
//
// Parse checks packet shape, not endpoint limits or RPC state. Call
// Data.ValidateBounds before accepting data into an assembled message. Reserved
// fields are preserved and no assumptions are made about peer port/ID policy.
func Parse(b []byte) (Packet, error) {
	if len(b) < CommonSize {
		return nil, ErrTruncated
	}
	h := parseCommon(b)
	size, ok := HeaderSize(h.Type)
	if !ok {
		return nil, fmt.Errorf("%w: 0x%02x", ErrType, h.Type)
	}
	if len(b) < size {
		return nil, ErrTruncated
	}
	switch h.Type {
	case DataType:
		return Data{Common: h, MessageLength: binary.BigEndian.Uint32(b[28:32]),
			Incoming: binary.BigEndian.Uint32(b[32:36]), Ack: parseAck(b[36:46]),
			CutoffVersion: binary.BigEndian.Uint16(b[46:48]), Retransmit: b[48],
			Padding: [3]byte{b[49], b[50], b[51]}, SegmentOffset: binary.BigEndian.Uint32(b[52:56]),
			Payload: b[DataSize:]}, nil
	case GrantType:
		return Grant{Common: h, Offset: binary.BigEndian.Uint32(b[28:32]), Priority: b[32]}, nil
	case ResendType:
		return Resend{Common: h, Offset: binary.BigEndian.Uint32(b[28:32]), Length: binary.BigEndian.Uint32(b[32:36]), Priority: b[36]}, nil
	case RPCUnknownType:
		return RPCUnknown{Common: h}, nil
	case BusyType:
		return Busy{Common: h}, nil
	case CutoffsType:
		p := Cutoffs{Common: h, CutoffVersion: binary.BigEndian.Uint16(b[60:62])}
		for i := range p.UnscheduledCutoffs {
			p.UnscheduledCutoffs[i] = binary.BigEndian.Uint32(b[28+4*i : 32+4*i])
		}
		return p, nil
	case FreezeType:
		return Freeze{Common: h}, nil
	case NeedAckType:
		return NeedAck{Common: h}, nil
	case AckType:
		p := Ack{Common: h, NumAcks: binary.BigEndian.Uint16(b[28:30])}
		if p.NumAcks > MaxAcks {
			return nil, ErrAckCount
		}
		for i := range p.Acks {
			p.Acks[i] = parseAck(b[30+i*AckRecordSize : 40+i*AckRecordSize])
		}
		return p, nil
	}
	panic("unreachable packet type")
}

func marshalCommon(b []byte, h Common) {
	binary.BigEndian.PutUint16(b[0:2], h.SourcePort)
	binary.BigEndian.PutUint16(b[2:4], h.DestinationPort)
	binary.BigEndian.PutUint32(b[4:8], h.Sequence)
	copy(b[8:11], h.Ack[:])
	b[11], b[12], b[13] = byte(h.Type), h.DataOffset, h.Flags
	binary.BigEndian.PutUint16(b[14:16], h.Window)
	binary.BigEndian.PutUint16(b[16:18], h.Checksum)
	binary.BigEndian.PutUint16(b[18:20], h.Urgent)
	binary.BigEndian.PutUint64(b[20:28], h.SenderID)
}

func marshalAck(b []byte, ack AckRecord) {
	binary.BigEndian.PutUint64(b[0:8], ack.ClientID)
	binary.BigEndian.PutUint16(b[8:10], ack.ServerPort)
}

// Marshal serializes a complete fixed header and, for DATA, its payload.
// It preserves unused fields exactly. Use NewCommon to set native defaults.
// Common.Type must agree with the concrete packet type; no payload policy is
// imposed here because the endpoint owns maximum-message and segment limits.
func Marshal(packet Packet) ([]byte, error) {
	// Normalize pointers without dereferencing a typed nil.
	switch p := packet.(type) {
	case *Data:
		if p == nil {
			return nil, ErrType
		}
		return Marshal(*p)
	case *Grant:
		if p == nil {
			return nil, ErrType
		}
		return Marshal(*p)
	case *Resend:
		if p == nil {
			return nil, ErrType
		}
		return Marshal(*p)
	case *RPCUnknown:
		if p == nil {
			return nil, ErrType
		}
		return Marshal(*p)
	case *Busy:
		if p == nil {
			return nil, ErrType
		}
		return Marshal(*p)
	case *Cutoffs:
		if p == nil {
			return nil, ErrType
		}
		return Marshal(*p)
	case *Freeze:
		if p == nil {
			return nil, ErrType
		}
		return Marshal(*p)
	case *NeedAck:
		if p == nil {
			return nil, ErrType
		}
		return Marshal(*p)
	case *Ack:
		if p == nil {
			return nil, ErrType
		}
		return Marshal(*p)
	case *CommonPacket:
		if p == nil {
			return nil, ErrType
		}
		return Marshal(*p)
	}
	if packet == nil {
		return nil, ErrType
	}
	h := packet.Header()
	size, ok := HeaderSize(h.Type)
	if !ok {
		return nil, ErrType
	}
	var expected Type
	var payload []byte
	switch p := packet.(type) {
	case Data:
		expected, payload = DataType, p.Payload
	case Grant:
		expected = GrantType
	case Resend:
		expected = ResendType
	case RPCUnknown:
		expected = RPCUnknownType
	case Busy:
		expected = BusyType
	case Cutoffs:
		expected = CutoffsType
	case Freeze:
		expected = FreezeType
	case NeedAck:
		expected = NeedAckType
	case Ack:
		expected = AckType
		if p.NumAcks > MaxAcks {
			return nil, ErrAckCount
		}
	case CommonPacket:
		if size != CommonSize {
			return nil, ErrType
		}
		expected = h.Type
	default:
		return nil, ErrType
	}
	if h.Type != expected {
		return nil, ErrType
	}
	if len(payload) > int(^uint(0)>>1)-size {
		return nil, ErrDataBounds
	}
	b := make([]byte, size+len(payload))
	marshalCommon(b, h)
	switch p := packet.(type) {
	case Data:
		binary.BigEndian.PutUint32(b[28:32], p.MessageLength)
		binary.BigEndian.PutUint32(b[32:36], p.Incoming)
		marshalAck(b[36:46], p.Ack)
		binary.BigEndian.PutUint16(b[46:48], p.CutoffVersion)
		b[48] = p.Retransmit
		copy(b[49:52], p.Padding[:])
		binary.BigEndian.PutUint32(b[52:56], p.SegmentOffset)
		copy(b[56:], p.Payload)
	case Grant:
		binary.BigEndian.PutUint32(b[28:32], p.Offset)
		b[32] = p.Priority
	case Resend:
		binary.BigEndian.PutUint32(b[28:32], p.Offset)
		binary.BigEndian.PutUint32(b[32:36], p.Length)
		b[36] = p.Priority
	case Cutoffs:
		for i, cutoff := range p.UnscheduledCutoffs {
			binary.BigEndian.PutUint32(b[28+i*4:32+i*4], cutoff)
		}
		binary.BigEndian.PutUint16(b[60:62], p.CutoffVersion)
	case Ack:
		binary.BigEndian.PutUint16(b[28:30], p.NumAcks)
		for i, ack := range p.Acks {
			marshalAck(b[30+i*AckRecordSize:40+i*AckRecordSize], ack)
		}
	}
	return b, nil
}
