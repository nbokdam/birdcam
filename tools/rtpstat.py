#!/usr/bin/env python3
"""Read an RFC 4571 framed RTP stream (2-byte BE length + RTP packet) from stdin
(or a file arg) and report per-payload-type stats and A/V drift.

drift(pt) = (rtp_ts - first_ts)/clock - (arrival - first_arrival)
Prints a line every --interval seconds:
  t=<wall s> v: pkts fps kbps drift | a: pkts drift | av=<video_drift - audio_drift>
Also reports first-IDR time (relative to --t0 epoch if given, else to process start),
max packet size, NAL types, SPS/PPS presence on IDR.
Optionally --dump FILE writes the raw stream through (for later decoding).
"""
import argparse, struct, sys, time

CLOCK = {96: 90000, 111: 48000}

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--interval", type=float, default=10.0)
    ap.add_argument("--duration", type=float, default=0)
    ap.add_argument("--t0", type=float, default=None, help="epoch secs when pipeline was launched")
    ap.add_argument("--dump", default=None)
    a = ap.parse_args()
    t0 = a.t0 if a.t0 is not None else time.time()
    inp = sys.stdin.buffer
    dump = open(a.dump, "wb") if a.dump else None
    st = {}
    first_idr = None
    idr_count = 0; idr_with_sps = 0
    frames = 0; last_report = time.time(); win_bytes = 0; win_frames = 0
    maxsz = 0; badpt = set(); seqerr = {}
    last_seq = {}
    pending_sps = False
    last_frame_ts = None
    start = time.time()
    while True:
        hdr = inp.read(2)
        if len(hdr) < 2:
            break
        (n,) = struct.unpack(">H", hdr)
        pkt = inp.read(n)
        if len(pkt) < n:
            break
        now = time.time()
        if dump: dump.write(hdr + pkt)
        maxsz = max(maxsz, n)
        pt = pkt[1] & 0x7F
        marker = pkt[1] >> 7
        seq, ts = struct.unpack(">HI", pkt[2:8])
        cc = pkt[0] & 0x0F
        off = 12 + 4 * cc
        if pkt[0] & 0x10:
            el = struct.unpack(">H", pkt[off + 2:off + 4])[0]
            off += 4 + 4 * el
        if pt not in CLOCK:
            badpt.add(pt); continue
        s = st.setdefault(pt, {"ts0": ts, "arr0": now, "pkts": 0, "ext": 0, "last": ts, "drift": 0.0, "bytes": 0, "mind": 1e9, "maxd": -1e9})
        # unwrap
        d = (ts - s["last"]) & 0xFFFFFFFF
        if d > 0x7FFFFFFF: d -= 0x100000000
        s["ext"] += d; s["last"] = ts
        if pt in last_seq and ((seq - last_seq[pt]) & 0xFFFF) != 1:
            seqerr[pt] = seqerr.get(pt, 0) + 1
        last_seq[pt] = seq
        s["pkts"] += 1; s["bytes"] += n
        s["drift"] = s["ext"] / CLOCK[pt] - (now - s["arr0"])
        if pt == 96:
            win_bytes += n
            p = pkt[off:]
            if not p:
                continue
            nt = p[0] & 0x1F
            nals = []
            if nt == 28:  # FU-A
                if p[1] & 0x80: nals.append(p[1] & 0x1F)
            elif nt == 24:  # STAP-A
                i = 1
                while i + 2 <= len(p):
                    l = struct.unpack(">H", p[i:i+2])[0]; i += 2
                    if l == 0 or i >= len(p): break
                    nals.append(p[i] & 0x1F); i += l
            else:
                nals.append(nt)
            for x in nals:
                if x == 7: pending_sps = True
                if x == 5:
                    idr_count += 1
                    if pending_sps: idr_with_sps += 1
                    pending_sps = False
                    if first_idr is None:
                        first_idr = now - t0
                        print(f"FIRST_IDR {first_idr:.3f}s after t0", flush=True)
                if x == 1: pending_sps = False
            if marker:
                frames += 1; win_frames += 1
        if now - last_report >= a.interval:
            el = now - last_report
            v = st.get(96); au = st.get(111)
            line = f"t={now-start:7.1f}"
            if v: line += f" v: fps={win_frames/el:5.1f} kbps={win_bytes*8/el/1000:7.0f} drift={v['drift']:+.3f}"
            if au: line += f" a: drift={au['drift']:+.3f}"
            if v and au: line += f" av={v['drift']-au['drift']:+.3f}"
            print(line, flush=True)
            last_report = now; win_bytes = 0; win_frames = 0
        if a.duration and now - start > a.duration:
            break
    el = time.time() - start
    print(f"SUMMARY elapsed={el:.1f}s frames={frames} fps={frames/max(el,1e-9):.2f} maxpkt={maxsz} idr={idr_count} idr_with_sps={idr_with_sps} seqgaps={seqerr} unknown_pt={sorted(badpt)}")
    for pt, s in st.items():
        print(f"  pt={pt} pkts={s['pkts']} kbps={s['bytes']*8/max(el,1e-9)/1000:.0f} final_drift={s['drift']:+.3f}")
    if 96 in st and 111 in st:
        print(f"  AV_DRIFT final={st[96]['drift']-st[111]['drift']:+.3f}s")
    sys.stdout.flush()

if __name__ == "__main__":
    main()
