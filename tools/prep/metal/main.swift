// prep-metal: the records stage of `prep` on an Apple GPU (Metal).
//
//   prep-metal spec.json        IXPT point chunks -> a prep batch (IXP1 parts)
//   prep-metal --selftest [n]   the sort and the sums against the CPU
//
// Per batch of chunks (up to `batchPoints` points): keys from cells, a stable
// radix sort of (key, index), segment heads, voxel moments in double-float,
// payload gather. Then one sort of all voxel records, the zone levels from
// the bottom up (each level sums the runs of the level below), and the parts.
// See docs/ingest/pipeline.md, section 6.
import CryptoKit
import Foundation
import Metal

func fail(_ s: String) -> Never {
    FileHandle.standardError.write((s + "\n").data(using: .utf8)!)
    exit(1)
}

func now() -> Double { Date().timeIntervalSince1970 }

let TG = 256, TILE = 2048

final class GPU {
    let device: MTLDevice
    let queue: MTLCommandQueue
    var pipes: [String: MTLComputePipelineState] = [:]
    var gpuSeconds = 0.0

    init(source: String) {
        guard let d = MTLCreateSystemDefaultDevice() else { fail("no Metal device") }
        device = d
        guard let q = d.makeCommandQueue() else { fail("no command queue") }
        queue = q
        let o = MTLCompileOptions()
        if #available(macOS 15.0, *) {
            o.mathMode = .safe  // double-float needs every rounding as written
            o.mathFloatingPointFunctions = .precise
        } else {
            o.fastMathEnabled = false
        }
        let lib: MTLLibrary
        do { lib = try d.makeLibrary(source: source, options: o) } catch { fail("metal compile: \(error)") }
        for name in ["make_keys", "varying_bits", "upsweep", "scan_exclusive", "downsweep", "prefix_keys", "count_heads",
                     "write_heads", "leaf_moments", "gather_payload", "segment_sums", "scan_rows", "crown_peaks", "crown_grow"] {
            guard let f = lib.makeFunction(name: name) else { fail("no kernel \(name)") }
            do { pipes[name] = try d.makeComputePipelineState(function: f) } catch { fail("pipeline \(name): \(error)") }
            if pipes[name]!.threadExecutionWidth != 32 { fail("SIMD width \(pipes[name]!.threadExecutionWidth), the sort needs 32") }
            if pipes[name]!.maxTotalThreadsPerThreadgroup < 1024 && name.hasPrefix("scan") { fail("\(name) needs 1024 threads") }
        }
    }

    func buffer(_ bytes: Int) -> MTLBuffer {
        guard let b = device.makeBuffer(length: max(bytes, 16), options: .storageModeShared) else { fail("out of GPU memory (\(bytes) B)") }
        return b
    }

    // run encodes kernels in one serial encoder (each dispatch sees the
    // previous one's writes), commits and waits.
    func run(_ body: (MTLComputeCommandEncoder) -> Void) {
        guard let cb = queue.makeCommandBuffer(), let enc = cb.makeComputeCommandEncoder() else { fail("no command buffer") }
        body(enc)
        enc.endEncoding()
        cb.commit()
        cb.waitUntilCompleted()
        if let e = cb.error { fail("GPU: \(e)") }
        gpuSeconds += cb.gpuEndTime - cb.gpuStartTime
    }

    func threads(_ e: MTLComputeCommandEncoder, _ name: String, _ n: Int) {
        let p = pipes[name]!
        e.setComputePipelineState(p)
        e.dispatchThreads(MTLSize(width: max(n, 1), height: 1, depth: 1),
                          threadsPerThreadgroup: MTLSize(width: min(p.maxTotalThreadsPerThreadgroup, 256), height: 1, depth: 1))
    }

    func groups(_ e: MTLComputeCommandEncoder, _ name: String, _ n: Int, _ tg: Int = TG) {
        e.setComputePipelineState(pipes[name]!)
        e.dispatchThreadgroups(MTLSize(width: max(n, 1), height: 1, depth: 1), threadsPerThreadgroup: MTLSize(width: tg, height: 1, depth: 1))
    }
}

func bytes<T>(_ e: MTLComputeCommandEncoder, _ v: T, _ i: Int) {
    withUnsafeBytes(of: v) { e.setBytes($0.baseAddress!, length: $0.count, index: i) }
}

func array(_ e: MTLComputeCommandEncoder, _ a: [UInt8], _ i: Int) {
    a.withUnsafeBytes { e.setBytes($0.baseAddress!, length: max($0.count, 1), index: i) }
}

struct SortParams { var n: UInt32; var shift: UInt32; var tiles: UInt32; var pad: UInt32 = 0 }

// Sorter: a stable LSD radix sort of 64-bit keys carrying a 32-bit index.
// Keys go in keyA/idxA; sort returns the buffers that hold the result.
final class Sorter {
    let gpu: GPU
    let cap: Int
    let keyA, keyB, idxA, idxB, hist, totals, varying, count, starts: MTLBuffer
    var passes = 0
    var profile = false
    var times: [String: Double] = [:]

    init(_ gpu: GPU, capacity: Int) {
        self.gpu = gpu
        cap = capacity
        let tiles = (capacity + TILE - 1) / TILE
        keyA = gpu.buffer(8 * capacity); keyB = gpu.buffer(8 * capacity)
        idxA = gpu.buffer(4 * capacity); idxB = gpu.buffer(4 * capacity)
        hist = gpu.buffer(4 * (256 * tiles + 1))
        totals = gpu.buffer(4 * 257)
        varying = gpu.buffer(8)
        count = gpu.buffer(4 * (tiles + 1))
        starts = gpu.buffer(4 * (capacity + 1))
    }

    func sort(_ n: Int) -> (MTLBuffer, MTLBuffer) {
        if n <= 1 { return (keyA, idxA) }
        let tiles = (n + TILE - 1) / TILE
        varying.contents().storeBytes(of: UInt64(0), as: UInt64.self)
        gpu.run { e in
            e.setBuffer(keyA, offset: 0, index: 0); e.setBuffer(varying, offset: 0, index: 1); bytes(e, UInt32(n), 2)
            gpu.threads(e, "varying_bits", n)
        }
        let v = varying.contents().load(as: UInt64.self)
        var src = (keyA, idxA), dst = (keyB, idxB)
        // profile: one command buffer per kernel, GPU time per kernel name
        let one: (String, (MTLComputeCommandEncoder) -> Void) -> Void = { name, body in
            if self.profile {
                let before = self.gpu.gpuSeconds
                self.gpu.run(body)
                self.times[name, default: 0] += self.gpu.gpuSeconds - before
            }
        }
        if profile {
            for d in 0..<8 where (v >> UInt64(8 * d)) & 255 != 0 {
                let p = SortParams(n: UInt32(n), shift: UInt32(8 * d), tiles: UInt32(tiles))
                let (s0, d0) = (src, dst)
                one("upsweep") { e in e.setBuffer(s0.0, offset: 0, index: 0); e.setBuffer(self.hist, offset: 0, index: 1); bytes(e, p, 2); self.gpu.groups(e, "upsweep", tiles) }
                one("scan") { e in
                    e.setBuffer(self.hist, offset: 0, index: 0); e.setBuffer(self.totals, offset: 0, index: 1); bytes(e, UInt32(tiles), 2)
                    self.gpu.groups(e, "scan_rows", 256, 1024)
                    e.setBuffer(self.totals, offset: 0, index: 0); bytes(e, UInt32(256), 1)
                    self.gpu.groups(e, "scan_exclusive", 1, 1024)
                }
                one("downsweep") { e in
                    e.setBuffer(s0.0, offset: 0, index: 0); e.setBuffer(s0.1, offset: 0, index: 1)
                    e.setBuffer(d0.0, offset: 0, index: 2); e.setBuffer(d0.1, offset: 0, index: 3)
                    e.setBuffer(self.hist, offset: 0, index: 4); bytes(e, p, 5); e.setBuffer(self.totals, offset: 0, index: 6)
                    self.gpu.groups(e, "downsweep", tiles)
                }
                swap(&src, &dst)
                passes += 1
            }
            return src
        }
        gpu.run { e in
            for d in 0..<8 where (v >> UInt64(8 * d)) & 255 != 0 {
                let p = SortParams(n: UInt32(n), shift: UInt32(8 * d), tiles: UInt32(tiles))
                encodePass(e, p, src, dst, tiles)
                swap(&src, &dst)
                passes += 1
            }
        }
        return src
    }

    // One digit: counts per tile, a scan of each digit's row (one threadgroup
    // per digit), a scan of the 256 digit totals, the stable scatter.
    func encodePass(_ e: MTLComputeCommandEncoder, _ p: SortParams, _ src: (MTLBuffer, MTLBuffer), _ dst: (MTLBuffer, MTLBuffer), _ tiles: Int) {
        e.setBuffer(src.0, offset: 0, index: 0); e.setBuffer(hist, offset: 0, index: 1); bytes(e, p, 2)
        gpu.groups(e, "upsweep", tiles)
        e.setBuffer(hist, offset: 0, index: 0); e.setBuffer(totals, offset: 0, index: 1); bytes(e, UInt32(tiles), 2)
        gpu.groups(e, "scan_rows", 256, 1024)
        e.setBuffer(totals, offset: 0, index: 0); bytes(e, UInt32(256), 1)
        gpu.groups(e, "scan_exclusive", 1, 1024)
        e.setBuffer(src.0, offset: 0, index: 0); e.setBuffer(src.1, offset: 0, index: 1)
        e.setBuffer(dst.0, offset: 0, index: 2); e.setBuffer(dst.1, offset: 0, index: 3)
        e.setBuffer(hist, offset: 0, index: 4); bytes(e, p, 5); e.setBuffer(totals, offset: 0, index: 6)
        gpu.groups(e, "downsweep", tiles)
    }

    // heads finds the runs of equal keys in sorted keys: starts[0..<segs]
    // and starts[segs] = n.
    func heads(_ keys: MTLBuffer, _ n: Int) -> Int {
        let tiles = (n + TILE - 1) / TILE
        gpu.run { e in
            e.setBuffer(keys, offset: 0, index: 0); e.setBuffer(count, offset: 0, index: 1); bytes(e, UInt32(n), 2)
            gpu.groups(e, "count_heads", tiles)
            e.setBuffer(count, offset: 0, index: 0); bytes(e, UInt32(tiles), 1)
            gpu.groups(e, "scan_exclusive", 1, 1024)
            e.setBuffer(keys, offset: 0, index: 0); e.setBuffer(count, offset: 0, index: 1)
            e.setBuffer(starts, offset: 0, index: 2); bytes(e, UInt32(n), 3)
            gpu.groups(e, "write_heads", tiles)
        }
        let segs = Int(count.contents().load(fromByteOffset: 4 * tiles, as: UInt32.self))
        starts.contents().storeBytes(of: UInt32(n), toByteOffset: 4 * segs, as: UInt32.self)
        return segs
    }
}

// --- the part file (internal/prep, IXP1) ----------------------------------
// CRC-32 (IEEE), slicing by 8.
let crcTables: [[UInt32]] = {
    var t = [[UInt32]](repeating: [UInt32](repeating: 0, count: 256), count: 8)
    for i in 0..<256 {
        var c = UInt32(i)
        for _ in 0..<8 { c = (c & 1) != 0 ? (c >> 1) ^ 0xEDB8_8320 : c >> 1 }
        t[0][i] = c
    }
    for i in 0..<256 { for k in 1..<8 { t[k][i] = (t[k - 1][i] >> 8) ^ t[0][Int(t[k - 1][i] & 0xFF)] } }
    return t
}()

func crc32(_ d: [UInt8]) -> UInt32 {
    var c: UInt32 = 0xFFFF_FFFF
    let t0 = crcTables[0], t1 = crcTables[1], t2 = crcTables[2], t3 = crcTables[3]
    let t4 = crcTables[4], t5 = crcTables[5], t6 = crcTables[6], t7 = crcTables[7]
    d.withUnsafeBytes { (p: UnsafeRawBufferPointer) in
        let n = p.count
        var i = 0
        while i + 8 <= n {
            let lo = c ^ (UInt32(p[i]) | UInt32(p[i + 1]) << 8 | UInt32(p[i + 2]) << 16 | UInt32(p[i + 3]) << 24)
            let hi = UInt32(p[i + 4]) | UInt32(p[i + 5]) << 8 | UInt32(p[i + 6]) << 16 | UInt32(p[i + 7]) << 24
            let a = t7[Int(lo & 0xFF)] ^ t6[Int((lo >> 8) & 0xFF)] ^ t5[Int((lo >> 16) & 0xFF)] ^ t4[Int(lo >> 24)]
            let b = t3[Int(hi & 0xFF)] ^ t2[Int((hi >> 8) & 0xFF)] ^ t1[Int((hi >> 16) & 0xFF)] ^ t0[Int(hi >> 24)]
            c = a ^ b
            i += 8
        }
        while i < n {
            c = t0[Int((c ^ UInt32(p[i])) & 0xFF)] ^ (c >> 8)
            i += 1
        }
    }
    return c ^ 0xFFFF_FFFF
}

extension Data {
    mutating func u16(_ v: UInt16) { var x = v.littleEndian; Swift.withUnsafeBytes(of: &x) { append(contentsOf: $0) } }
    mutating func u32(_ v: UInt32) { var x = v.littleEndian; Swift.withUnsafeBytes(of: &x) { append(contentsOf: $0) } }
    mutating func u64(_ v: UInt64) { var x = v.littleEndian; Swift.withUnsafeBytes(of: &x) { append(contentsOf: $0) } }
    mutating func f64(_ v: Double) { u64(v.bitPattern) }
    mutating func pad8() { while count % 8 != 0 { append(0) } }
}

let alphabet = Array("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_")

func location(_ key: UInt64, _ depth: Int) -> String {
    if depth == 0 { return "@" }
    return String((0..<depth).map { alphabet[Int((key >> UInt64(58 - 6 * $0)) & 63)] })
}


// PartWriter fills a block as typed columns and writes each column with
// one copy.
let b64 = Array("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/".utf8)

final class PartWriter {
    let fh: FileHandle
    var sha = SHA256()
    let m: Int, payloads: Bool
    var off: UInt64 = 0, rows: UInt64 = 0
    var index = [UInt8]()
    var blocks: UInt32 = 0
    var heads = [UInt8](), ids = [UInt8](), idEnd = [UInt32](), payEnd = [UInt64](), weights = [Double]()
    var metrics: [[Double]], pay = [UInt8]()
    var jsonBase64 = false  // write each payload as the JSON string "<base64>" (what the node stores today)

    func appendQuotedBase64(_ p: UnsafeRawBufferPointer) {
        let n = p.count, at = pay.count, len = 2 + 4 * ((n + 2) / 3)
        pay.append(contentsOf: repeatElement(61, count: len))  // "=" pads what the loop does not write
        pay.withUnsafeMutableBufferPointer { o in
            b64.withUnsafeBufferPointer { t in
                var j = at
                o[j] = 34; j += 1
                var i = 0
                while i + 3 <= n {
                    let v = UInt32(p[i]) << 16 | UInt32(p[i + 1]) << 8 | UInt32(p[i + 2])
                    o[j] = t[Int(v >> 18)]; o[j + 1] = t[Int((v >> 12) & 63)]; o[j + 2] = t[Int((v >> 6) & 63)]; o[j + 3] = t[Int(v & 63)]
                    i += 3; j += 4
                }
                if n - i > 0 {
                    let v = UInt32(p[i]) << 16 | (n - i == 2 ? UInt32(p[i + 1]) << 8 : 0)
                    o[j] = t[Int(v >> 18)]; o[j + 1] = t[Int((v >> 12) & 63)]
                    if n - i == 2 { o[j + 2] = t[Int((v >> 6) & 63)] }
                }
                o[at + len - 1] = 34
            }
        }
    }

    init(_ path: String, m: Int, payloads: Bool) {
        FileManager.default.createFile(atPath: path, contents: nil)
        guard let f = FileHandle(forWritingAtPath: path) else { fail("cannot write \(path)") }
        fh = f
        self.m = m
        self.payloads = payloads
        metrics = Array(repeating: [], count: m)
        var h = Data("IXP1".utf8)
        h.u16(1); h.u16(UInt16(m)); h.u32(payloads ? 1 : 0); h.u32(0)
        write(h)
    }

    func write(_ d: Data) {
        fh.write(d)
        sha.update(data: d)
        off += UInt64(d.count)
    }

    func add(_ key: UInt64, _ depth: Int, _ id: [UInt8], _ weight: Double, _ ms: UnsafeBufferPointer<Double>, _ payload: UnsafeRawBufferPointer?) {
        for i in 0..<8 { heads.append(UInt8((key >> UInt64(56 - 8 * i)) & 255)) }
        heads.append(0); heads.append(0); heads.append(0); heads.append(0); heads.append(UInt8(depth))
        ids.append(contentsOf: id)
        idEnd.append(UInt32(ids.count))
        if let p = payload {
            if jsonBase64 { appendQuotedBase64(p) } else { pay.append(contentsOf: p) }
        }
        payEnd.append(UInt64(pay.count))
        weights.append(weight)
        for j in 0..<m { metrics[j].append(ms[j]) }
        if weights.count == 65536 || pay.count >= 64 << 20 { flush() }
    }

    func flush() {
        let n = weights.count
        if n == 0 { return }
        var b = [UInt8]()
        b.reserveCapacity(64 + n * (13 + 4 + 8 + 8 * (1 + m)) + ids.count + pay.count)
        func raw<T>(_ a: [T]) { a.withUnsafeBytes { b.append(contentsOf: $0) } }
        func pad() { while b.count % 8 != 0 { b.append(0) } }
        raw([UInt32(n).littleEndian, UInt32(ids.count).littleEndian])
        raw([UInt64(pay.count).littleEndian])
        b.append(contentsOf: heads); pad()
        raw(idEnd); pad()
        if payloads { raw(payEnd) }
        raw(weights)
        for col in metrics { raw(col) }
        b.append(contentsOf: ids); pad()
        b.append(contentsOf: pay); pad()
        let c = crc32(b)
        raw([c, UInt32(0)])
        index.append(contentsOf: heads[0..<13])
        index.append(contentsOf: [0, 0, 0])
        withUnsafeBytes(of: off.littleEndian) { index.append(contentsOf: $0) }
        withUnsafeBytes(of: UInt32(b.count).littleEndian) { index.append(contentsOf: $0) }
        withUnsafeBytes(of: UInt32(n).littleEndian) { index.append(contentsOf: $0) }
        blocks += 1
        rows += UInt64(n)
        heads.removeAll(keepingCapacity: true); ids.removeAll(keepingCapacity: true); idEnd.removeAll(keepingCapacity: true)
        payEnd.removeAll(keepingCapacity: true); weights.removeAll(keepingCapacity: true); pay.removeAll(keepingCapacity: true)
        for j in 0..<m { metrics[j].removeAll(keepingCapacity: true) }
        write(Data(b))
    }

    func close() -> (sha: String, bytes: UInt64) {
        flush()
        let at = off
        let ix = Data(index)
        write(ix)
        var f = Data()
        f.u64(at); f.u64(rows); f.u32(blocks); f.u32(crc32(index))
        f.append(Data("IXP1".utf8)); f.u32(0)
        write(f)
        fh.closeFile()
        return (sha.finalize().map { String(format: "%02x", $0) }.joined(), off)
    }
}

// --- the vegetation records stage -----------------------------------------
struct Chunk {
    let path: String
    let n: Int
    let width: Int
    let bits: Int
}

func readHeader(_ path: String) -> Chunk {
    guard let fh = FileHandle(forReadingAtPath: path) else { fail("cannot read \(path)") }
    defer { fh.closeFile() }
    let h = fh.readData(ofLength: 32)
    if h.count < 32 || String(data: h.prefix(4), encoding: .ascii) != "IXPT" { fail("\(path): not an IXPT chunk") }
    let n = h.withUnsafeBytes { $0.load(fromByteOffset: 8, as: UInt64.self) }
    let w = h.withUnsafeBytes { $0.load(fromByteOffset: 16, as: UInt32.self) }
    let bits = h.withUnsafeBytes { $0.load(fromByteOffset: 20, as: UInt32.self) }
    return Chunk(path: path, n: Int(n), width: Int(w), bits: Int(bits))
}

// pread fills dst with bytes [at, at+count) of a file.
func pread(_ fd: Int32, _ dst: UnsafeMutableRawPointer, _ count: Int, _ at: Int) {
    var done = 0
    while done < count {
        let r = Darwin.pread(fd, dst + done, count - done, off_t(at + done))
        if r <= 0 { fail("short read") }
        done += r
    }
}

struct LeafParams { var stride: UInt32; var segs: UInt32; var width: UInt32; var rgbAt: UInt32 }
struct KeyParams { var n: UInt32; var depth: UInt32; var bits: UInt32; var stride: UInt32 }

func runSpec(_ specPath: String, _ gpu: GPU) {
    guard let raw = FileManager.default.contents(atPath: specPath),
          let spec = try? JSONSerialization.jsonObject(with: raw) as? [String: Any] else { fail("bad spec \(specPath)") }
    let t0 = now()
    let depth = spec["depth"] as! Int, bits = spec["bits"] as! Int
    let voxel = spec["voxel"] as! Double, origin = (spec["origin"] as! NSNumber).doubleValue
    let mask = (spec["mask"] as! [Int]), offsets = (spec["offset"] as! [Int]).map { UInt8($0) }
    let chunkPaths = spec["chunks"] as! [String]
    let out = spec["out"] as! String
    let rid = Array((spec["id"] as! String).utf8)
    let batchPoints = (spec["batchPoints"] as? Int) ?? 8_000_000
    // Space mask -> which axis and which bit of that axis each key bit takes.
    var axisOf = [UInt8](), bitOf = [UInt8](), used = [bits, bits, bits]
    for j in 0..<(6 * depth) {
        let a = mask[j]
        used[a] -= 1
        axisOf.append(UInt8(a)); bitOf.append(UInt8(used[a]))
    }
    let chunks = chunkPaths.map(readHeader)
    let width = chunks.first?.width ?? 6
    var batches: [[Chunk]] = [], cur: [Chunk] = [], curN = 0
    for c in chunks {
        if curN + c.n > batchPoints && !cur.isEmpty { batches.append(cur); cur = []; curN = 0 }
        cur.append(c); curN += c.n
    }
    if !cur.isEmpty { batches.append(cur) }
    let cap = max(batches.map { $0.reduce(0) { $0 + $1.n } }.max() ?? 1, 1)
    let sorter = Sorter(gpu, capacity: cap)
    let cellBuf = gpu.buffer(4 * cap), dBuf = gpu.buffer(24 * cap), payBuf = gpu.buffer(width * cap), gathered = gpu.buffer(width * cap)
    var sumsBuf = gpu.buffer(72 * 1024), extraBuf = gpu.buffer(20 * 1024)

    // records, in batch order
    var recKey = [UInt64](), recMet = [Double](), recPayAt = [Int](), recPayLen = [Int]()
    var arena = [UInt8]()
    var tRead = 0.0, tKeys = 0.0, tSort = 0.0, tHeads = 0.0, tLeaf = 0.0, tCPU = 0.0, points = 0
    for batch in batches {
        var a = now()
        var off = 0
        for c in batch {
            let fd = open(c.path, O_RDONLY)
            if fd < 0 { fail("open \(c.path)") }
            pread(fd, cellBuf.contents() + 4 * off, 4 * c.n, 32)
            for col in 0..<6 { pread(fd, dBuf.contents() + 4 * (col * cap + off), 4 * c.n, 32 + 4 * c.n + 4 * col * c.n) }
            pread(fd, payBuf.contents() + width * off, width * c.n, 32 + 28 * c.n)
            close(fd)
            off += c.n
        }
        let n = off
        points += n
        var b = now(); tRead += b - a; a = b
        gpu.run { e in
            e.setBuffer(cellBuf, offset: 0, index: 0); e.setBuffer(sorter.keyA, offset: 0, index: 1); e.setBuffer(sorter.idxA, offset: 0, index: 2)
            bytes(e, KeyParams(n: UInt32(n), depth: UInt32(depth), bits: UInt32(bits), stride: UInt32(cap)), 3)
            array(e, axisOf, 4); array(e, bitOf, 5); array(e, offsets, 6)
            gpu.threads(e, "make_keys", n)
        }
        b = now(); tKeys += b - a; a = b
        let (keys, idx) = sorter.sort(n)
        b = now(); tSort += b - a; a = b
        let segs = sorter.heads(keys, n)
        b = now(); tHeads += b - a; a = b
        if sumsBuf.length < 72 * segs { sumsBuf = gpu.buffer(72 * segs); extraBuf = gpu.buffer(20 * segs) }
        gpu.run { e in
            e.setBuffer(sorter.starts, offset: 0, index: 0); e.setBuffer(idx, offset: 0, index: 1); e.setBuffer(dBuf, offset: 0, index: 2)
            e.setBuffer(payBuf, offset: 0, index: 3); e.setBuffer(sumsBuf, offset: 0, index: 4); e.setBuffer(extraBuf, offset: 0, index: 5)
            bytes(e, LeafParams(stride: UInt32(cap), segs: UInt32(segs), width: UInt32(width), rgbAt: 3), 6)
            gpu.threads(e, "leaf_moments", segs)
            e.setBuffer(idx, offset: 0, index: 0); e.setBuffer(payBuf, offset: 0, index: 1); e.setBuffer(gathered, offset: 0, index: 2)
            bytes(e, SIMD2<UInt32>(UInt32(n), UInt32(width)), 3)
            gpu.threads(e, "gather_payload", n)
        }
        b = now(); tLeaf += b - a; a = b
        // records: absolute moments in float64 from the sums about each voxel centre
        let sums = sumsBuf.contents().bindMemory(to: Float.self, capacity: 18 * segs)
        let extra = extraBuf.contents().bindMemory(to: UInt32.self, capacity: 5 * segs)
        let starts = sorter.starts.contents().bindMemory(to: UInt32.self, capacity: segs + 1)
        let keyp = keys.contents().bindMemory(to: UInt64.self, capacity: n)
        let cells = cellBuf.contents().bindMemory(to: UInt32.self, capacity: n)
        let base = arena.count
        arena.append(contentsOf: UnsafeRawBufferPointer(start: gathered.contents(), count: width * n))
        let mk = UInt32((1 << bits) - 1)
        for s in 0..<segs {
            let st = Int(starts[s])
            recKey.append(keyp[st])
            recPayAt.append(base + width * st)
            recPayLen.append(width * Int(extra[5 * s]))
            var v = [Double](repeating: 0, count: 9)
            for j in 0..<9 { v[j] = Double(sums[18 * s + 2 * j]) + Double(sums[18 * s + 2 * j + 1]) }
            let cnt = Double(extra[5 * s])
            let c = cells[Int(extra[5 * s + 4])]
            let cx = Double((c >> UInt32(2 * bits)) & mk) * voxel + origin + voxel / 2
            let cy = Double((c >> UInt32(bits)) & mk) * voxel + origin + voxel / 2
            let cz = Double(c & mk) * voxel + origin + voxel / 2
            let sx = v[0], sy = v[1], sz = v[2]
            let mx: Double = cnt * cx + sx
            let my: Double = cnt * cy + sy
            let mz: Double = cnt * cz + sz
            let mxx: Double = cnt * cx * cx + 2 * cx * sx + v[3]
            let mxy: Double = cnt * cx * cy + cx * sy + cy * sx + v[4]
            let mxz: Double = cnt * cx * cz + cx * sz + cz * sx + v[5]
            let myy: Double = cnt * cy * cy + 2 * cy * sy + v[6]
            let myz: Double = cnt * cy * cz + cy * sz + cz * sy + v[7]
            let mzz: Double = cnt * cz * cz + 2 * cz * sz + v[8]
            let r: Double = Double(extra[5 * s + 1]) / 255
            let g: Double = Double(extra[5 * s + 2]) / 255
            let bl: Double = Double(extra[5 * s + 3]) / 255
            recMet.append(cnt); recMet.append(mx); recMet.append(my); recMet.append(mz)
            recMet.append(mxx); recMet.append(mxy); recMet.append(mxz); recMet.append(myy); recMet.append(myz); recMet.append(mzz)
            recMet.append(r); recMet.append(g); recMet.append(bl)
        }
        b = now(); tCPU += b - a
    }
    let m = 13
    let nrec = recKey.count
    // one sort of every record (voxels nest in source files: no key twice)
    var a = now()
    let rs = Sorter(gpu, capacity: max(nrec, 1))
    recKey.withUnsafeBytes { rs.keyA.contents().copyMemory(from: $0.baseAddress!, byteCount: $0.count) }
    let ip = rs.idxA.contents().bindMemory(to: UInt32.self, capacity: nrec)
    for i in 0..<nrec { ip[i] = UInt32(i) }
    let (sk, si) = rs.sort(nrec)
    let order = Array(UnsafeBufferPointer(start: si.contents().bindMemory(to: UInt32.self, capacity: nrec), count: nrec))
    let skeys = Array(UnsafeBufferPointer(start: sk.contents().bindMemory(to: UInt64.self, capacity: nrec), count: nrec))
    for i in 1..<max(nrec, 1) where skeys[i] <= skeys[i - 1] { fail("record keys not unique at \(location(skeys[i], depth))") }
    var b = now(); let tGlobal = b - a; a = b

    // zones, bottom up on the GPU: level d sums the runs of level d + 1
    let zd = spec["zoneDepths"] as! [Int]
    let zm = m + 1  // count, then the metrics
    var levelKeys = skeys
    var vals = [Float](repeating: 0, count: 2 * zm * nrec)
    for (i, r) in order.enumerated() {
        vals[2 * i * zm] = 1
        for j in 1..<zm {
            let x = recMet[Int(r) * m + j - 1]
            let hi = Float(x)
            vals[2 * (i * zm + j)] = hi
            vals[2 * (i * zm + j) + 1] = Float(x - Double(hi))
        }
    }
    var zones: [(UInt64, Int, [Double])] = []
    let zs = Sorter(gpu, capacity: max(nrec, 1))
    var valBuf = gpu.buffer(4 * vals.count)
    vals.withUnsafeBytes { valBuf.contents().copyMemory(from: $0.baseAddress!, byteCount: $0.count) }
    var d = depth - 1
    while d >= zd[0] {
        let n = levelKeys.count
        levelKeys.withUnsafeBytes { zs.keyB.contents().copyMemory(from: $0.baseAddress!, byteCount: $0.count) }
        gpu.run { e in
            e.setBuffer(zs.keyB, offset: 0, index: 0); e.setBuffer(zs.keyA, offset: 0, index: 1)
            bytes(e, SIMD2<UInt32>(UInt32(n), UInt32(6 * d)), 2)
            gpu.threads(e, "prefix_keys", n)
        }
        let segs = zs.heads(zs.keyA, n)
        let outBuf = gpu.buffer(8 * zm * segs)
        gpu.run { e in
            e.setBuffer(zs.starts, offset: 0, index: 0); e.setBuffer(valBuf, offset: 0, index: 1); e.setBuffer(outBuf, offset: 0, index: 2)
            bytes(e, SIMD2<UInt32>(UInt32(segs), UInt32(zm)), 3)
            gpu.threads(e, "segment_sums", segs)
        }
        let st = zs.starts.contents().bindMemory(to: UInt32.self, capacity: segs + 1)
        let pk = zs.keyA.contents().bindMemory(to: UInt64.self, capacity: n)
        let ov = outBuf.contents().bindMemory(to: Float.self, capacity: 2 * zm * segs)
        var next = [UInt64]()
        for s in 0..<segs {
            let k = pk[Int(st[s])]
            next.append(k)
            if d <= zd[1] {
                var row = [Double](repeating: 0, count: zm)
                for j in 0..<zm {
                    let hi = Double(ov[2 * (s * zm + j)])
                    let lo = Double(ov[2 * (s * zm + j) + 1])
                    row[j] = hi + lo
                }
                zones.append((k, d, row))
            }
        }
        levelKeys = next
        valBuf = outBuf
        d -= 1
    }
    zones.sort { $0.0 != $1.0 ? $0.0 < $1.0 : $0.1 < $1.1 }
    b = now(); let tZones = b - a; a = b

    // parts: end at the first zone boundary of splitDepth past partBytes
    try? FileManager.default.createDirectory(atPath: out, withIntermediateDirectories: true)
    let split = spec["splitDepth"] as! Int, partBytes = spec["partBytes"] as! Int
    var parts: [[String: Any]] = []
    var w: PartWriter? = nil, size = 0, first = 0, payloadBytes = 0
    let jsonPayload = (spec["payloadEncoding"] as? String ?? "binary") == "json"
    let zmask: UInt64 = split == 0 ? 0 : ~((UInt64(1) << UInt64(64 - 6 * split)) - 1)
    func closePart(_ last: Int) {
        let (sha, bytes) = w!.close()
        parts.append(["file": String(format: "part-%05d.ixp", parts.count), "records": last - first + 1, "bytes": bytes,
                      "first": location(skeys[first], depth) + "/" + String(decoding: rid, as: UTF8.self),
                      "last": location(skeys[last], depth) + "/" + String(decoding: rid, as: UTF8.self), "sha256": sha])
    }
    arena.withUnsafeBytes { ar in
        for i in 0..<nrec {
            let r = Int(order[i])
            if w != nil && size >= partBytes && (skeys[i] & zmask) != (skeys[i - 1] & zmask) {
                closePart(i - 1)
                w = nil
            }
            if w == nil {
                w = PartWriter(out + String(format: "/part-%05d.ixp", parts.count), m: m, payloads: true)
                w!.jsonBase64 = jsonPayload
                size = 0; first = i
            }
            let p = UnsafeRawBufferPointer(rebasing: ar[recPayAt[r]..<(recPayAt[r] + recPayLen[r])])
            let plen = jsonPayload ? 2 + 4 * ((p.count + 2) / 3) : p.count
            recMet.withUnsafeBufferPointer { mp in
                w!.add(skeys[i], depth, rid, 1, UnsafeBufferPointer(rebasing: mp[(r * m)..<(r * m + m)]), p)
            }
            size += 13 + rid.count + 8 * (1 + m) + plen
            payloadBytes += plen
        }
    }
    if w != nil { closePart(nrec - 1) }
    var man: [String: Any] = ["format": "indexus-prep-v1", "collection": spec["collection"]!, "metrics": m,
                              "metricNames": spec["metricNames"]!, "records": nrec, "payloadBytes": payloadBytes,
                              "payloadEncoding": jsonPayload ? "json" : "binary",
                              "recordDepths": [depth, depth], "splitDepth": split, "parts": parts,
                              "inputs": spec["inputs"]!, "id": spec["batchID"]!]
    let zw = PartWriter(out + "/zones.ixp", m: zm, payloads: false)
    for z in zones { z.2.withUnsafeBufferPointer { zw.add(z.0, z.1, [], z.2[0], $0, nil) } }
    let (zsha, _) = zw.close()
    man["zones"] = ["file": "zones.ixp", "depths": zd, "records": zones.count, "sha256": zsha]
    let mj = try! JSONSerialization.data(withJSONObject: man, options: [.prettyPrinted, .sortedKeys])
    FileManager.default.createFile(atPath: out + "/manifest.json", contents: mj + Data("\n".utf8))
    b = now()
    let tWrite = b - a
    var secs: [String: Double] = ["read": tRead, "key": tKeys, "sort": tSort, "heads": tHeads, "leaf": tLeaf, "cpu": tCPU]
    secs["global"] = tGlobal; secs["zones"] = tZones; secs["write"] = tWrite; secs["total"] = b - t0; secs["gpu"] = gpu.gpuSeconds
    let gpuStages: Double = tKeys + tSort + tHeads + tLeaf
    var stats: [String: Any] = ["stage": "records", "backend": "metal", "records": nrec, "points": points, "batches": batches.count]
    stats["sortPasses"] = sorter.passes; stats["zones"] = zones.count
    stats["seconds"] = secs.mapValues { ($0 * 1000).rounded() / 1000 }
    var ru = rusage()
    getrusage(RUSAGE_SELF, &ru)
    stats["peak_rss_mb"] = Int(ru.ru_maxrss) / 1_000_000
    stats["gpu_allocated_mb"] = gpu.device.currentAllocatedSize / 1_000_000
    stats["points_per_s_gpu_stages"] = (Double(points) / max(gpuStages, 1e-9)).rounded()
    print(String(data: try! JSONSerialization.data(withJSONObject: stats, options: [.sortedKeys]), encoding: .utf8)!)
}

// --- self test: sort and sums against the CPU -----------------------------
func selftest(_ n: Int, _ gpu: GPU) {
    var rng = SystemRandomNumberGenerator()
    let s = Sorter(gpu, capacity: n)
    let kp = s.keyA.contents().bindMemory(to: UInt64.self, capacity: n)
    let ip = s.idxA.contents().bindMemory(to: UInt32.self, capacity: n)
    var keys = [UInt64](repeating: 0, count: n)
    for i in 0..<n {
        keys[i] = (UInt64.random(in: 0..<(1 << 22), using: &rng) << 34) | 0x0A00_0000_0000_0000  // 22 varying bits, a constant top
        kp[i] = keys[i]; ip[i] = UInt32(i)
    }
    let a = now()
    let (sk, si) = s.sort(n)
    let t = now() - a
    let ok = sk.contents().bindMemory(to: UInt64.self, capacity: n), oi = si.contents().bindMemory(to: UInt32.self, capacity: n)
    var bad = 0
    var seen = [Bool](repeating: false, count: n)
    for i in 0..<n {
        if keys[Int(oi[i])] != ok[i] { bad += 1 }
        if seen[Int(oi[i])] { bad += 1 }
        seen[Int(oi[i])] = true
        if i > 0 && (ok[i] < ok[i - 1] || (ok[i] == ok[i - 1] && oi[i] < oi[i - 1])) { bad += 1 }
    }
    let segs = s.heads(sk, n)
    var want = 1
    for i in 1..<n where ok[i] != ok[i - 1] { want += 1 }
    let st = s.starts.contents().bindMemory(to: UInt32.self, capacity: segs + 1)
    var badHeads = segs == want ? 0 : 1
    for j in 0..<segs where !(st[j] == 0 || ok[Int(st[j])] != ok[Int(st[j]) - 1]) { badHeads += 1 }
    // sums: double-float against Double on random offsets
    let m = 1, rows = 1 << 16
    var vals = [Float](repeating: 0, count: 2 * rows), exact = 0.0
    for i in 0..<rows {
        let x = Double.random(in: -1000...1000, using: &rng)
        let hi = Float(x)
        vals[2 * i] = hi; vals[2 * i + 1] = Float(x - Double(hi)); exact += Double(hi) + Double(vals[2 * i + 1])
    }
    let vb = gpu.buffer(4 * vals.count), ob = gpu.buffer(8), sb = gpu.buffer(8)
    vals.withUnsafeBytes { vb.contents().copyMemory(from: $0.baseAddress!, byteCount: $0.count) }
    sb.contents().storeBytes(of: SIMD2<UInt32>(0, UInt32(rows)), as: SIMD2<UInt32>.self)
    gpu.run { e in
        e.setBuffer(sb, offset: 0, index: 0); e.setBuffer(vb, offset: 0, index: 1); e.setBuffer(ob, offset: 0, index: 2)
        bytes(e, SIMD2<UInt32>(1, UInt32(m)), 3)
        gpu.threads(e, "segment_sums", 1)
    }
    let o = ob.contents().bindMemory(to: Float.self, capacity: 2)
    let got = Double(o[0]) + Double(o[1])
    // the same sort again, one command buffer per kernel, for the GPU time of each
    for i in 0..<n { kp[i] = keys[i]; ip[i] = UInt32(i) }
    s.profile = true
    _ = s.sort(n)
    let out: [String: Any] = ["selftest": n, "kernelSeconds": s.times, "sortErrors": bad, "headErrors": badHeads, "segments": segs, "passes": s.passes,
                              "sortSeconds": t, "keysPerSecond": Double(n) / t, "dfSum": got, "exactSum": exact,
                              "dfRelativeError": abs(got - exact) / abs(exact)]
    print(String(data: try! JSONSerialization.data(withJSONObject: out, options: [.sortedKeys]), encoding: .utf8)!)
    if bad + badHeads > 0 { exit(1) }
}

// --- crowns: peaks and region growing on the n x n grid (crowns.py) ---
func readInto(_ path: String, _ b: MTLBuffer, _ bytes: Int) {
    let fd = open(path, O_RDONLY)
    if fd < 0 { fail("open \(path)") }
    pread(fd, b.contents(), bytes, 0)
    close(fd)
}

func writeFrom(_ path: String, _ b: MTLBuffer, _ bytes: Int) {
    FileManager.default.createFile(atPath: path, contents: Data(bytes: b.contents(), count: bytes))
}

func crownPeaks(_ dir: String, _ n: Int, _ gpu: GPU) {
    let cells = n * n
    let h = gpu.buffer(4 * cells), hj = gpu.buffer(4 * cells), peak = gpu.buffer(cells)
    readInto(dir + "/peaks-h.f32", h, 4 * cells)
    readInto(dir + "/peaks-hj.f32", hj, 4 * cells)
    let a = now()
    gpu.run { e in
        e.setBuffer(h, offset: 0, index: 0); e.setBuffer(hj, offset: 0, index: 1); e.setBuffer(peak, offset: 0, index: 2)
        bytes(e, UInt32(n), 3)
        gpu.threads(e, "crown_peaks", cells)
    }
    let t = now() - a
    writeFrom(dir + "/peaks.u8", peak, cells)
    print("{\"peaksSeconds\":\(t)}")
}

func crownGrow(_ dir: String, _ n: Int, _ topCount: Int, _ gpu: GPU) {
    let cells = n * n
    let h = gpu.buffer(4 * cells), labA = gpu.buffer(4 * cells), labB = gpu.buffer(4 * cells), top = gpu.buffer(4 * topCount)
    readInto(dir + "/grow-h.f32", h, 4 * cells)
    readInto(dir + "/grow-label.i32", labA, 4 * cells)
    readInto(dir + "/grow-top.f32", top, 4 * topCount)
    let per = 8
    let counters = gpu.buffer(4 * per)
    var src = labA, dst = labB, iterations = 0, done = false
    let a = now()
    while !done && iterations < 200 {
        memset(counters.contents(), 0, 4 * per)
        var bufs: [MTLBuffer] = []
        gpu.run { e in
            for k in 0..<per {
                e.setBuffer(h, offset: 0, index: 0); e.setBuffer(src, offset: 0, index: 1); e.setBuffer(dst, offset: 0, index: 2)
                e.setBuffer(top, offset: 0, index: 3); e.setBuffer(counters, offset: 4 * k, index: 4); bytes(e, UInt32(n), 5)
                gpu.threads(e, "crown_grow", cells)
                bufs.append(dst)
                swap(&src, &dst)
            }
        }
        let c = counters.contents().bindMemory(to: UInt32.self, capacity: per)
        for k in 0..<per {
            if c[k] == 0 { done = true; src = bufs[k]; break }  // step k changed nothing: its output is the result
            iterations += 1
            if iterations == 200 { src = bufs[k]; done = true; break }
        }
    }
    let t = now() - a
    writeFrom(dir + "/grow-label.i32", src, 4 * cells)
    print("{\"iterations\":\(iterations),\"growSeconds\":\(t)}")
}

let exe = URL(fileURLWithPath: CommandLine.arguments[0]).resolvingSymlinksInPath().deletingLastPathComponent()
let srcPath = ProcessInfo.processInfo.environment["PREP_METAL_SOURCE"] ?? exe.appendingPathComponent("prep.metal").path
guard let source = try? String(contentsOfFile: srcPath, encoding: .utf8) else { fail("cannot read \(srcPath)") }
let gpu = GPU(source: source)
let args = CommandLine.arguments
if args.count == 4 && args[1] == "--peaks" {
    crownPeaks(args[2], Int(args[3])!, gpu)
} else if args.count == 5 && args[1] == "--grow" {
    crownGrow(args[2], Int(args[3])!, Int(args[4])!, gpu)
} else if args.count >= 2 && args[1] == "--selftest" {
    for n in (args.count > 2 ? [Int(args[2])!] : [1000, 100_000, 2_000_000, 8_000_000]) { selftest(n, gpu) }
} else if args.count == 2 {
    runSpec(args[1], gpu)
} else {
    fail("usage: prep-metal spec.json | --selftest [n]")
}
