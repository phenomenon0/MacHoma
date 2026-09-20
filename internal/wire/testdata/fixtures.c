/* Compile the actual pinned header; do not duplicate its struct definitions.
 * cc -std=c11 -Wall -Wextra -Werror -Itestdata/shim testdata/fixtures.c -o fixtures
 */
#include <arpa/inet.h>
#include <stddef.h>
#include <stdio.h>
#include <string.h>
#include "upstream/homa_wire.h"

#define LAYOUT_BEGIN(type) printf("\"" #type "\":{\"size\":%zu,\"offsets\":{", sizeof(struct type))
#define FIELD(type, field) printf("\"" #field "\":%zu", offsetof(struct type, field))
#define NEXT_FIELD(type, field) printf(","); FIELD(type, field)
#define LAYOUT_END() printf("}}")
#define COMMON_LAYOUT(type) LAYOUT_BEGIN(type); FIELD(type, common); LAYOUT_END()

static struct homa_common_hdr common(unsigned int type)
{
    struct homa_common_hdr h;
    memset(&h, 0, sizeof(h));
    h.sport = htons(0x1234);
    h.dport = htons(0xabcd);
    h.sequence = htonl(0x10203040);
    h.ack[0] = 0x11; h.ack[1] = 0x22; h.ack[2] = 0x33;
    h.type = type;
    h.doff = type == DATA ? 0xd0 : 0x50;
    h.flags = 0x42;
    h.window = htons(0x5678);
    h.checksum = htons(0x9abc);
    h.urgent = htons(HOMA_HIJACK_URGENT);
    h.sender_id = fixture_be64(UINT64_C(0x0102030405060708));
    return h;
}

static void hex_packet(const char *name, const void *packet, size_t length)
{
    const unsigned char *p = packet;
    printf("\"%s\":\"", name);
    for (size_t i = 0; i < length; i++) printf("%02x", p[i]);
    printf("\"");
}

int main(void)
{
    printf("{\"revision\":\"d8914b8a57aa48c19a2c8130484961585bcbe53c\",\"layouts\":{");
    LAYOUT_BEGIN(homa_common_hdr);
    FIELD(homa_common_hdr, sport); NEXT_FIELD(homa_common_hdr, dport);
    NEXT_FIELD(homa_common_hdr, sequence); NEXT_FIELD(homa_common_hdr, ack);
    NEXT_FIELD(homa_common_hdr, type); NEXT_FIELD(homa_common_hdr, doff);
    NEXT_FIELD(homa_common_hdr, flags); NEXT_FIELD(homa_common_hdr, window);
    NEXT_FIELD(homa_common_hdr, checksum); NEXT_FIELD(homa_common_hdr, urgent);
    NEXT_FIELD(homa_common_hdr, sender_id); LAYOUT_END(); printf(",");
    LAYOUT_BEGIN(homa_ack); FIELD(homa_ack, client_id); NEXT_FIELD(homa_ack, server_port); LAYOUT_END(); printf(",");
    LAYOUT_BEGIN(homa_seg_hdr); FIELD(homa_seg_hdr, offset); LAYOUT_END(); printf(",");
    LAYOUT_BEGIN(homa_data_hdr); FIELD(homa_data_hdr, common);
    NEXT_FIELD(homa_data_hdr, message_length); NEXT_FIELD(homa_data_hdr, incoming);
    NEXT_FIELD(homa_data_hdr, ack); NEXT_FIELD(homa_data_hdr, cutoff_version);
    NEXT_FIELD(homa_data_hdr, retransmit); NEXT_FIELD(homa_data_hdr, pad);
    NEXT_FIELD(homa_data_hdr, seg); LAYOUT_END(); printf(",");
    LAYOUT_BEGIN(homa_grant_hdr); FIELD(homa_grant_hdr, common);
    NEXT_FIELD(homa_grant_hdr, offset); NEXT_FIELD(homa_grant_hdr, priority); LAYOUT_END(); printf(",");
    LAYOUT_BEGIN(homa_resend_hdr); FIELD(homa_resend_hdr, common);
    NEXT_FIELD(homa_resend_hdr, offset); NEXT_FIELD(homa_resend_hdr, length);
    NEXT_FIELD(homa_resend_hdr, priority); LAYOUT_END(); printf(",");
    COMMON_LAYOUT(homa_rpc_unknown_hdr); printf(",");
    COMMON_LAYOUT(homa_busy_hdr); printf(",");
    LAYOUT_BEGIN(homa_cutoffs_hdr); FIELD(homa_cutoffs_hdr, common);
    NEXT_FIELD(homa_cutoffs_hdr, unsched_cutoffs); NEXT_FIELD(homa_cutoffs_hdr, cutoff_version); LAYOUT_END(); printf(",");
    COMMON_LAYOUT(homa_freeze_hdr); printf(",");
    COMMON_LAYOUT(homa_need_ack_hdr); printf(",");
    LAYOUT_BEGIN(homa_ack_hdr); FIELD(homa_ack_hdr, common);
    NEXT_FIELD(homa_ack_hdr, num_acks); NEXT_FIELD(homa_ack_hdr, acks); LAYOUT_END();
    printf("},\"constants\":{\"DATA\":%d,\"GRANT\":%d,\"RESEND\":%d,\"RPC_UNKNOWN\":%d,\"BUSY\":%d,\"CUTOFFS\":%d,\"FREEZE\":%d,\"NEED_ACK\":%d,\"ACK\":%d,\"HOMA_MAX_PRIORITIES\":%d,\"HOMA_MAX_ACKS_PER_PKT\":%d,\"HOMA_MIN_PKT_LENGTH\":%d,\"HOMA_MAX_HEADER\":%d,\"HOMA_HIJACK_URGENT\":%d},\"packets\":{",
        DATA, GRANT, RESEND, RPC_UNKNOWN, BUSY, CUTOFFS, FREEZE, NEED_ACK, ACK,
        HOMA_MAX_PRIORITIES, HOMA_MAX_ACKS_PER_PKT, HOMA_MIN_PKT_LENGTH, HOMA_MAX_HEADER, HOMA_HIJACK_URGENT);

    unsigned char data_packet[sizeof(struct homa_data_hdr) + 7];
    struct homa_data_hdr data;
    memset(&data, 0, sizeof(data));
    data.common = common(DATA);
    data.message_length = htonl(1024); data.incoming = htonl(512);
    data.ack.client_id = fixture_be64(UINT64_C(0x1020304050607080));
    data.ack.server_port = htons(0x2233);
    data.cutoff_version = htons(0x4567); data.retransmit = 1;
    data.pad[0] = 0x12; data.pad[1] = 0x34; data.pad[2] = 0x56;
    data.seg.offset = htonl(17);
    memcpy(data_packet, &data, sizeof(data));
    memcpy(data_packet + sizeof(data), "payload", 7);
    hex_packet("DATA", data_packet, sizeof(data_packet)); printf(",");
    struct homa_grant_hdr grant = { .common = common(GRANT), .offset = htonl(0x11223344), .priority = 7 };
    hex_packet("GRANT", &grant, sizeof(grant)); printf(",");
    struct homa_resend_hdr resend = { .common = common(RESEND), .offset = htonl(0x23456789), .length = htonl(0xffffffff), .priority = 6 };
    hex_packet("RESEND", &resend, sizeof(resend)); printf(",");
    struct homa_rpc_unknown_hdr unknown = { .common = common(RPC_UNKNOWN) };
    hex_packet("RPC_UNKNOWN", &unknown, sizeof(unknown)); printf(",");
    struct homa_busy_hdr busy = { .common = common(BUSY) };
    hex_packet("BUSY", &busy, sizeof(busy)); printf(",");
    struct homa_cutoffs_hdr cutoffs = { .common = common(CUTOFFS), .cutoff_version = htons(0x5678) };
    for (unsigned int i = 0; i < HOMA_MAX_PRIORITIES; i++) cutoffs.unsched_cutoffs[i] = htonl(0x12340000 + i);
    hex_packet("CUTOFFS", &cutoffs, sizeof(cutoffs)); printf(",");
    struct homa_freeze_hdr freeze = { .common = common(FREEZE) };
    hex_packet("FREEZE", &freeze, sizeof(freeze)); printf(",");
    struct homa_need_ack_hdr need_ack = { .common = common(NEED_ACK) };
    hex_packet("NEED_ACK", &need_ack, sizeof(need_ack)); printf(",");
    struct homa_ack_hdr ack = { .common = common(ACK), .num_acks = htons(2) };
    for (unsigned int i = 0; i < HOMA_MAX_ACKS_PER_PKT; i++) {
        ack.acks[i].client_id = fixture_be64(UINT64_C(0x1020304050607000) + i * 2);
        ack.acks[i].server_port = htons(0x4000 + i);
    }
    hex_packet("ACK", &ack, sizeof(ack));
    printf("}}\n");
    return 0;
}
