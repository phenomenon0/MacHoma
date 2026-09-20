/* Minimal userspace types to compile the unmodified upstream header for tests. */
#ifndef HOMA_FIXTURE_SKB_H
#define HOMA_FIXTURE_SKB_H
#include <stdint.h>
typedef uint8_t u8;
typedef uint32_t u32;
typedef uint64_t u64;
typedef uint16_t __be16;
typedef uint32_t __be32;
typedef uint64_t __be64;
#define __packed __attribute__((packed))
struct sk_buff { unsigned int len; };
static inline unsigned int skb_transport_offset(struct sk_buff *skb)
{
    (void) skb;
    return 0;
}
static inline uint64_t fixture_be64(uint64_t value)
{
#if __BYTE_ORDER__ == __ORDER_LITTLE_ENDIAN__
    return __builtin_bswap64(value);
#else
    return value;
#endif
}
#define be64_to_cpu(value) fixture_be64(value)
#endif
