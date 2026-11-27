// Kernels of prep-metal (docs/ingest/pipeline.md, section 6): keys, an LSD
// radix sort of 64-bit keys with a payload index (stable, 8-bit digits,
// reduce-then-scan with SIMD-group ranking), segment heads, sums per segment
// in double-float (two f32: about 48 bits of mantissa) and payload gather.
#include <metal_stdlib>
using namespace metal;

constant uint TG = 256;               // threads per threadgroup
constant uint ROUNDS = 8;             // keys per thread per tile
constant uint TILE = TG * ROUNDS;     // keys per tile
constant uint SG = TG / 32;           // SIMD groups per threadgroup

struct KeyParams { uint n; uint depth; uint bits; uint stride; };

// Location bits of a packed cell (x << 2b | y << b | z), left aligned in
// 64 bits: Space._encode of the SDK on integer cells, with the collection's
// per-character offset.
kernel void make_keys(device const uint* cell [[buffer(0)]],
                      device ulong* key [[buffer(1)]],
                      device uint* idx [[buffer(2)]],
                      constant KeyParams& p [[buffer(3)]],
                      constant uchar* axisOf [[buffer(4)]],
                      constant uchar* bitOf [[buffer(5)]],
                      constant uchar* offset [[buffer(6)]],
                      uint i [[thread_position_in_grid]]) {
    if (i >= p.n) return;
    uint c = cell[i];
    uint m = (1u << p.bits) - 1u;
    uint ax[3] = { (c >> (2u * p.bits)) & m, (c >> p.bits) & m, c & m };
    ulong k = 0;
    for (uint l = 0; l < p.depth; l++) {
        uint v = 0;
        for (uint b = 0; b < 6; b++) {
            uint j = l * 6 + b;
            v = (v << 1) | ((ax[axisOf[j]] >> bitOf[j]) & 1u);
        }
        k |= ulong((v + uint(offset[l])) & 63u) << (58u - 6u * l);
    }
    key[i] = k;
    idx[i] = i;
}

// Which bits differ from the first key: passes whose digit never changes are
// skipped.
kernel void varying_bits(device const ulong* key [[buffer(0)]],
                         device atomic_uint* out [[buffer(1)]],
                         constant uint& n [[buffer(2)]],
                         uint i [[thread_position_in_grid]]) {
    if (i >= n) return;
    ulong x = key[i] ^ key[0];
    if (x == 0) return;
    if (uint(x) != 0) atomic_fetch_or_explicit(&out[0], uint(x), memory_order_relaxed);
    if (uint(x >> 32) != 0) atomic_fetch_or_explicit(&out[1], uint(x >> 32), memory_order_relaxed);
}

struct SortParams { uint n; uint shift; uint tiles; uint pad; };

// Count of each digit in each tile, digit-major: hist[digit * tiles + tile].
kernel void upsweep(device const ulong* key [[buffer(0)]],
                    device uint* hist [[buffer(1)]],
                    constant SortParams& p [[buffer(2)]],
                    uint tid [[thread_index_in_threadgroup]],
                    uint tile [[threadgroup_position_in_grid]]) {
    threadgroup atomic_uint h[256];
    atomic_store_explicit(&h[tid], 0u, memory_order_relaxed);
    threadgroup_barrier(mem_flags::mem_threadgroup);
    uint base = tile * TILE;
    for (uint r = 0; r < ROUNDS; r++) {
        uint i = base + r * TG + tid;
        if (i < p.n) atomic_fetch_add_explicit(&h[uint(key[i] >> p.shift) & 255u], 1u, memory_order_relaxed);
    }
    threadgroup_barrier(mem_flags::mem_threadgroup);
    hist[tid * p.tiles + tile] = atomic_load_explicit(&h[tid], memory_order_relaxed);
}

// Exclusive scan of n counts in place, one threadgroup of 1024 threads; the
// total goes to a[n].
kernel void scan_exclusive(device uint* a [[buffer(0)]],
                           constant uint& n [[buffer(1)]],
                           uint tid [[thread_index_in_threadgroup]],
                           uint tgs [[threads_per_threadgroup]]) {
    threadgroup uint part[1024];
    uint per = (n + tgs - 1) / tgs;
    uint lo = min(tid * per, n), hi = min(lo + per, n);
    uint s = 0;
    for (uint i = lo; i < hi; i++) s += a[i];
    part[tid] = s;
    threadgroup_barrier(mem_flags::mem_threadgroup);
    for (uint off = 1; off < tgs; off <<= 1) {
        uint v = tid >= off ? part[tid - off] : 0u;
        threadgroup_barrier(mem_flags::mem_threadgroup);
        part[tid] += v;
        threadgroup_barrier(mem_flags::mem_threadgroup);
    }
    uint run = tid ? part[tid - 1] : 0u;
    for (uint i = lo; i < hi; i++) {
        uint v = a[i];
        a[i] = run;
        run += v;
    }
    if (tid == tgs - 1) a[n] = part[tid];
}

// Exclusive scan of each digit's row of tile counts (one threadgroup of 1024
// per digit); the row total goes to totals[digit].
kernel void scan_rows(device uint* hist [[buffer(0)]],
                      device uint* totals [[buffer(1)]],
                      constant uint& tiles [[buffer(2)]],
                      uint row [[threadgroup_position_in_grid]],
                      uint tid [[thread_index_in_threadgroup]],
                      uint tgs [[threads_per_threadgroup]],
                      uint lane [[thread_index_in_simdgroup]],
                      uint sg [[simdgroup_index_in_threadgroup]]) {
    threadgroup uint sgs[32];
    device uint* a = hist + row * tiles;
    uint per = (tiles + tgs - 1) / tgs;
    uint lo = min(tid * per, tiles), hi = min(lo + per, tiles);
    uint s = 0;
    for (uint i = lo; i < hi; i++) s += a[i];
    uint pre = simd_prefix_exclusive_sum(s);
    uint tot = simd_sum(s);
    if (lane == 0) sgs[sg] = tot;
    threadgroup_barrier(mem_flags::mem_threadgroup);
    if (sg == 0) {
        uint n = tgs / 32;
        uint v = lane < n ? sgs[lane] : 0u;
        uint p = simd_prefix_exclusive_sum(v);
        if (lane < n) sgs[lane] = p;
    }
    threadgroup_barrier(mem_flags::mem_threadgroup);
    uint run = sgs[sg] + pre;
    for (uint i = lo; i < hi; i++) {
        uint v = a[i];
        a[i] = run;
        run += v;
    }
    if (tid == tgs - 1) totals[row] = run;
}

inline ulong ballot(bool b) {
    return static_cast<ulong>(static_cast<simd_vote::vote_t>(simd_ballot(b)));
}

// Stable scatter of one digit: the rank of a key among the keys of its digit
// in its tile comes from SIMD ballots (the lanes that share its 8 digit
// bits), plus the counts of the earlier SIMD groups of the round and of the
// earlier rounds.
kernel void downsweep(device const ulong* keyIn [[buffer(0)]],
                      device const uint* idxIn [[buffer(1)]],
                      device ulong* keyOut [[buffer(2)]],
                      device uint* idxOut [[buffer(3)]],
                      device const uint* off [[buffer(4)]],
                      constant SortParams& p [[buffer(5)]],
                      device const uint* digitBase [[buffer(6)]],
                      uint tid [[thread_index_in_threadgroup]],
                      uint tile [[threadgroup_position_in_grid]],
                      uint lane [[thread_index_in_simdgroup]],
                      uint sg [[simdgroup_index_in_threadgroup]]) {
    threadgroup uint run[256];
    threadgroup uint sgc[SG][256];
    threadgroup uint base[256];
    run[tid] = 0;
    for (uint g = 0; g < SG; g++) sgc[g][tid] = 0;
    base[tid] = digitBase[tid] + off[tid * p.tiles + tile];
    threadgroup_barrier(mem_flags::mem_threadgroup);
    ulong below = (ulong(1) << lane) - 1;
    uint start = tile * TILE;
    for (uint r = 0; r < ROUNDS; r++) {
        uint i = start + r * TG + tid;
        bool valid = i < p.n;
        ulong k = valid ? keyIn[i] : 0;
        uint d = uint(k >> p.shift) & 255u;
        ulong peers = ballot(valid);
        for (uint b = 0; b < 8; b++) {
            bool bit = ((d >> b) & 1u) != 0;
            ulong v = ballot(bit);
            peers &= bit ? v : ~v;
        }
        uint rank = popcount(peers & below);
        if (valid && rank == 0) sgc[sg][d] = popcount(peers);
        threadgroup_barrier(mem_flags::mem_threadgroup);
        if (valid) {
            uint pos = base[d] + run[d] + rank;
            for (uint g = 0; g < sg; g++) pos += sgc[g][d];
            keyOut[pos] = k;
            idxOut[pos] = idxIn[i];
        }
        threadgroup_barrier(mem_flags::mem_threadgroup);
        uint add = 0;
        for (uint g = 0; g < SG; g++) {
            add += sgc[g][tid];
            sgc[g][tid] = 0;
        }
        run[tid] += add;
        threadgroup_barrier(mem_flags::mem_threadgroup);
    }
}

// Keys cut to a zone depth (keep the top 6d bits).
kernel void prefix_keys(device const ulong* key [[buffer(0)]],
                        device ulong* out [[buffer(1)]],
                        constant uint2& p [[buffer(2)]],   // n, kept bits
                        uint i [[thread_position_in_grid]]) {
    if (i >= p.x) return;
    out[i] = p.y == 0 ? 0 : (key[i] & ~((ulong(1) << (64u - p.y)) - 1));
}

inline bool isHead(device const ulong* key, uint i) { return i == 0 || key[i] != key[i - 1]; }

// Number of segment heads (first key of a run) in each tile.
kernel void count_heads(device const ulong* key [[buffer(0)]],
                        device uint* count [[buffer(1)]],
                        constant uint& n [[buffer(2)]],
                        uint tid [[thread_index_in_threadgroup]],
                        uint tile [[threadgroup_position_in_grid]]) {
    threadgroup atomic_uint c;
    if (tid == 0) atomic_store_explicit(&c, 0u, memory_order_relaxed);
    threadgroup_barrier(mem_flags::mem_threadgroup);
    uint s = 0;
    for (uint r = 0; r < ROUNDS; r++) {
        uint i = tile * TILE + r * TG + tid;
        if (i < n && isHead(key, i)) s++;
    }
    atomic_fetch_add_explicit(&c, s, memory_order_relaxed);
    threadgroup_barrier(mem_flags::mem_threadgroup);
    if (tid == 0) count[tile] = atomic_load_explicit(&c, memory_order_relaxed);
}

// Position of every head, in order: starts[segment] = first index.
kernel void write_heads(device const ulong* key [[buffer(0)]],
                        device const uint* off [[buffer(1)]],
                        device uint* starts [[buffer(2)]],
                        constant uint& n [[buffer(3)]],
                        uint tid [[thread_index_in_threadgroup]],
                        uint tile [[threadgroup_position_in_grid]],
                        uint sg [[simdgroup_index_in_threadgroup]]) {
    threadgroup uint sums[SG];
    uint run = off[tile];
    for (uint r = 0; r < ROUNDS; r++) {
        uint i = tile * TILE + r * TG + tid;
        uint f = (i < n && isHead(key, i)) ? 1u : 0u;
        uint before = simd_prefix_exclusive_sum(f);
        uint total = simd_sum(f);
        if (simd_is_first()) sums[sg] = total;
        threadgroup_barrier(mem_flags::mem_threadgroup);
        uint pos = run + before;
        uint all = 0;
        for (uint g = 0; g < SG; g++) {
            if (g < sg) pos += sums[g];
            all += sums[g];
        }
        if (f) starts[pos] = i;
        run += all;
        threadgroup_barrier(mem_flags::mem_threadgroup);
    }
}

// --- double-float arithmetic (Dekker; Knuth's two-sum) -------------------
inline float2 two_sum(float a, float b) {
    float s = a + b;
    float bb = s - a;
    return float2(s, (a - (s - bb)) + (b - bb));
}
inline float2 quick_two_sum(float a, float b) {
    float s = a + b;
    return float2(s, b - (s - a));
}
inline float2 df_add(float2 a, float2 b) {
    float2 s = two_sum(a.x, b.x);
    return quick_two_sum(s.x, s.y + a.y + b.y);
}
inline float2 df_mul(float2 a, float2 b) {
    float p = a.x * b.x;
    float e = fma(a.x, b.x, -p) + (a.x * b.y + a.y * b.x);
    return quick_two_sum(p, e);
}

struct LeafParams { uint stride; uint segs; uint width; uint rgbAt; };

// One thread per voxel: the 9 moments of the offsets from the voxel centre
// in double-float, the colour sums as integers, the count and one return
// (for the voxel's cell).
kernel void leaf_moments(device const uint* starts [[buffer(0)]],
                         device const uint* idx [[buffer(1)]],
                         device const float* d [[buffer(2)]],
                         device const uchar* pay [[buffer(3)]],
                         device float2* sums [[buffer(4)]],
                         device uint* extra [[buffer(5)]],
                         constant LeafParams& p [[buffer(6)]],
                         uint s [[thread_position_in_grid]]) {
    if (s >= p.segs) return;
    uint a = starts[s], b = starts[s + 1];
    float2 acc[9];
    for (uint j = 0; j < 9; j++) acc[j] = float2(0.0f);
    uint r = 0, g = 0, bl = 0;
    uint st = p.stride;
    for (uint i = a; i < b; i++) {
        uint q = idx[i];
        float2 x = float2(d[q], d[3 * st + q]);
        float2 y = float2(d[st + q], d[4 * st + q]);
        float2 z = float2(d[2 * st + q], d[5 * st + q]);
        acc[0] = df_add(acc[0], x);
        acc[1] = df_add(acc[1], y);
        acc[2] = df_add(acc[2], z);
        acc[3] = df_add(acc[3], df_mul(x, x));
        acc[4] = df_add(acc[4], df_mul(x, y));
        acc[5] = df_add(acc[5], df_mul(x, z));
        acc[6] = df_add(acc[6], df_mul(y, y));
        acc[7] = df_add(acc[7], df_mul(y, z));
        acc[8] = df_add(acc[8], df_mul(z, z));
        device const uchar* c = pay + ulong(q) * p.width + p.rgbAt;
        r += c[0];
        g += c[1];
        bl += c[2];
    }
    for (uint j = 0; j < 9; j++) sums[s * 9 + j] = acc[j];
    extra[s * 5] = b - a;
    extra[s * 5 + 1] = r;
    extra[s * 5 + 2] = g;
    extra[s * 5 + 3] = bl;
    extra[s * 5 + 4] = idx[a];
}

// Payload bytes in sorted order.
kernel void gather_payload(device const uint* idx [[buffer(0)]],
                           device const uchar* pay [[buffer(1)]],
                           device uchar* out [[buffer(2)]],
                           constant uint2& p [[buffer(3)]],   // n, width
                           uint i [[thread_position_in_grid]]) {
    if (i >= p.x) return;
    device const uchar* src = pay + ulong(idx[i]) * p.y;
    device uchar* dst = out + ulong(i) * p.y;
    for (uint k = 0; k < p.y; k++) dst[k] = src[k];
}

// Sums of m double-float columns over segments of rows: one level of zones
// from the level below it.
kernel void segment_sums(device const uint* starts [[buffer(0)]],
                         device const float2* vals [[buffer(1)]],
                         device float2* out [[buffer(2)]],
                         constant uint2& p [[buffer(3)]],   // segments, m
                         uint s [[thread_position_in_grid]]) {
    if (s >= p.x) return;
    uint a = starts[s], b = starts[s + 1];
    for (uint j = 0; j < p.y; j++) {
        float2 acc = float2(0.0f);
        for (uint i = a; i < b; i++) acc = df_add(acc, vals[i * p.y + j]);
        out[s * p.y + j] = acc;
    }
}

// --- crowns (crowns.py): local maxima and region growing on the 0.5 m grid ---

// A cell of height >= 3 m is a peak when it is the maximum of the square window
// of radius round((0.6 + 0.12 h) / 0.5), clipped to 2..8 cells (build_crowns.py).
kernel void crown_peaks(device const float* h [[buffer(0)]],
                        device const float* hj [[buffer(1)]],
                        device uchar* peak [[buffer(2)]],
                        constant uint& n [[buffer(3)]],
                        uint i [[thread_position_in_grid]]) {
    if (i >= n * n) return;
    peak[i] = 0;
    float hh = h[i];
    if (!(hh >= 3.0f)) return;
    int r = int(clamp(rint((0.6f + 0.12f * hh) / 0.5f), 2.0f, 8.0f));
    int cj = int(i / n), ci = int(i % n);
    float v = hj[i], m = v;
    for (int dj = -r; dj <= r; dj++) {
        int j = cj + dj;
        if (j < 0 || j >= int(n)) continue;
        for (int di = -r; di <= r; di++) {
            int k = ci + di;
            if (k < 0 || k >= int(n)) continue;
            m = max(m, hj[uint(j) * n + uint(k)]);
        }
    }
    peak[i] = v >= m ? 1 : 0;
}

// One step of region growing: an unlabelled cell >= 1.5 m takes the label of
// its highest labelled neighbour (south, north, west, east first on ties) that
// is at most 0.3 m below it... and whose crown top it is not under 45 % of.
kernel void crown_grow(device const float* h [[buffer(0)]],
                       device const int* labIn [[buffer(1)]],
                       device int* labOut [[buffer(2)]],
                       device const float* top [[buffer(3)]],
                       device atomic_uint* changed [[buffer(4)]],
                       constant uint& n [[buffer(5)]],
                       uint i [[thread_position_in_grid]]) {
    if (i >= n * n) return;
    int l0 = labIn[i];
    labOut[i] = l0;
    float hc = h[i];
    if (l0 != 0 || !(hc >= 1.5f)) return;
    int cj = int(i / n), ci = int(i % n);
    const int DJ[4] = {1, -1, 0, 0}, DI[4] = {0, 0, 1, -1};
    float bestH = -1.0f;
    int bestL = 0;
    for (int d = 0; d < 4; d++) {
        int j = cj + DJ[d], k = ci + DI[d];
        if (j < 0 || j >= int(n) || k < 0 || k >= int(n)) continue;
        uint nb = uint(j) * n + uint(k);
        int l = labIn[nb];
        float hn = h[nb];
        if (l > 0 && hc <= hn + 0.3f && hc >= max(1.5f, 0.45f * top[l]) && hn > bestH) {
            bestH = hn;
            bestL = l;
        }
    }
    if (bestL > 0) {
        labOut[i] = bestL;
        atomic_fetch_add_explicit(changed, 1u, memory_order_relaxed);
    }
}
