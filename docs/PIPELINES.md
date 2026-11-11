# BirdCam v2 – verified GStreamer pipelines

Result of the hardware/pipeline spike (2026-11-11) on a **Raspberry Pi 3 Model B**,
Raspberry Pi OS trixie arm64, kernel 6.18, **GStreamer 1.26.2**. Each pipeline here was
run on the Pi, and its stdout was parsed with `tools/rtpstat.py`. Some runs were also piped
over the network to a laptop and decoded there (H.264 to JPEG, Opus to WAV), and the
decoded output was checked by eye.

Cameras used:

| # | Camera | USB | Video nodes (`/dev/v4l/by-id/...`) | Mic |
|---|---|---|---|---|
| 1 | "H264 USB Camera" 32e4:9422 | 2.0 HS | `usb-USB_Camera_Manufacture_H264_USB_Camera_2020052801-video-index0` (MJPEG/YUYV), `...-video-index2` (H.264) | `hw:CARD=Camera,DEV=0`, mono S16LE 8–48 kHz |
| 2 | "GENERAL - UVC" 1902:3232 | 1.1 FS | `usb-Generic_GENERAL_-_UVC-video-index0` (MJPEG 640x480 / 320x240) | `hw:CARD=UVC,DEV=0`, mono S16LE **32 kHz only** (not 16 kHz) |

## 1. Pi packages

```sh
sudo apt-get install -y --no-install-recommends \
  gstreamer1.0-tools gstreamer1.0-plugins-base gstreamer1.0-plugins-good \
  gstreamer1.0-plugins-bad gstreamer1.0-alsa alsa-utils
# optional diagnostics: v4l-utils sysstat usbutils
```

| Element(s) | Package |
|---|---|
| `gst-launch-1.0` | tools |
| `opusenc`, `audioconvert`, `audioresample`, `audiorate` | plugins-base |
| `v4l2src`, `v4l2jpegdec`, `v4l2h264enc`, `v4l2convert`, `jpegdec`, `rtph264pay`, `rtpopuspay`, `rtpstreampay`, `funnel` (core), `fdsink` (core) | plugins-good (+ core) |
| `h264parse` | plugins-bad |
| `alsasrc` | gstreamer1.0-alsa |
| `amixer` (to unmute mics) | alsa-utils |

gstreamer1.0-libav is **not** needed. GStreamer exposes the bcm2835 codec devices like this:

| V4L2 card name | GStreamer element |
|---|---|
| `bcm2835-codec-decode` | `v4l2jpegdec` (also `v4l2h264dec`, ...) |
| `bcm2835-codec-encode` | `v4l2h264enc` |
| `bcm2835-codec-isp` | `v4l2convert` |
| `bcm2835-codec-image_fx` | `v4l2video18convert` (not used) |

The element names come from GStreamer's own device probing. The `device` property is
read-only, so a device can't be chosen in the pipeline string. If the Go agent wants to
check, it can run `gst-inspect-1.0 v4l2h264enc` (`device` default value), compare that
`/dev/videoN` with the card name found by its own `VIDIOC_QUERYCAP` scan, and then set
`hw_h264_encoder`.

## 2. Output contract (all modes)

- stdout carries RFC 4571 frames: 2-byte length followed by an RTP packet. Max RTP packet seen
  was 1200 bytes.
- Video: pt 96, H.264, 90 kHz. SPS+PPS are sent ahead of **every** IDR, which was checked on
  every IDR of every run.
- Audio: pt 111, Opus, 48 kHz RTP clock.
- **`-q` is mandatory.** Without it, gst-launch prints "Setting pipeline to PAUSED ..." on
  **stdout**, which breaks the framing. All logs and errors go to stderr.
- **Do not use `-e`** (see §7).

## 3. Mode `h264-passthrough` (camera 1, `...-video-index2`)

### Template, video + audio

```
gst-launch-1.0 -q \
  v4l2src device={video_device} \
  ! video/x-h264,width={width},height={height},framerate={fps}/1 \
  ! h264parse config-interval=-1 \
  ! rtph264pay pt=96 mtu=1200 config-interval=-1 aggregate-mode=zero-latency timestamp-offset=0 \
  ! funnel name=f ! rtpstreampay ! fdsink fd=1 sync=false \
  alsasrc device={audio_device} provide-clock=false slave-method=re-timestamp \
  ! audio/x-raw,rate={audio_rate},channels=1 \
  ! audiorate ! audioconvert ! audioresample ! audio/x-raw,rate=48000 \
  ! opusenc bitrate=32000 complexity=5 \
  ! rtpopuspay pt=111 timestamp-offset=0 ! f.
```

### Template, video only

Same as above without the `alsasrc ... ! f.` branch. The funnel with a single input is fine.

```
gst-launch-1.0 -q v4l2src device={video_device} ! video/x-h264,width={width},height={height},framerate={fps}/1 ! h264parse config-interval=-1 ! rtph264pay pt=96 mtu=1200 config-interval=-1 aggregate-mode=zero-latency timestamp-offset=0 ! funnel name=f ! rtpstreampay ! fdsink fd=1 sync=false
```

### What each part does

- The `video/x-h264,...` caps pick the resolution and frame rate on the H.264 node. The
  camera offers 1920x1080, 1280x720, 640x480, 640x360, 320x240 and 320x180 at 5–30 fps.
- `h264parse config-interval=-1` converts to AU alignment and re-inserts SPS/PPS. The camera
  already repeats them, but this makes the stream independent of the camera.
- `rtph264pay config-interval=-1` sends SPS/PPS with every IDR, as WebRTC join-in-progress
  needs. `aggregate-mode=zero-latency` avoids waiting to aggregate NALs.
  `mtu=1200` keeps packets within `MaxRTPPacketSize`.
- `timestamp-offset=0` on both payloaders makes RTP timestamp = pipeline running time ×
  clock rate, so video and audio timestamps share an origin. This is optional, but it lets
  the server relate the two streams and generate correct RTCP SRs if it wants to.
  Measured: the first audio timestamp was 0 and the first video timestamp was 0.16–1.1 s,
  which is the camera start-up time.
- For the audio flags (`provide-clock=false slave-method=re-timestamp` + `audiorate`), see §5.
- The camera's stream is **Main profile, level 3.1** (`profile-level-id=4d401f`), with no
  B-frames and an IDR every 30 frames. The server's SDP should allow 4d401f for this camera.
  The mjpeg-hwenc mode produces 42c01f / 42c028.

### Measured (camera 1)

| Setting | CPU (% of one core) | Real fps | Bitrate | First IDR on stdout |
|---|---|---|---|---|
| 720p30, video only | 8.2 % | 29.5 | 6.3 Mbit/s | 1.28 s |
| 1080p30, video only | 8.2 % | 29.5 | 6.3 Mbit/s | 1.20 s |
| 720p30 + audio (opus complexity 10) | 21.6–28 % | 29.5 | 6.45 + 0.035 Mbit/s | 1.31 s |

The bitrate is **about 6.3–6.5 Mbit/s at every resolution**, and there is no standard V4L2
control to change it (see §6). With `complexity=5`, Opus costs about 9 % of a core instead of
about 15 %, so expect roughly 17 % in total for passthrough + audio.

## 4. Mode `mjpeg-hwenc`

### Template A: hardware JPEG decode (recommended for camera 1 / USB 2.0 cameras)

```
gst-launch-1.0 -q \
  v4l2src device={video_device} \
  ! image/jpeg,width={width},height={height},framerate={fps}/1 \
  ! v4l2jpegdec capture-io-mode=dmabuf \
  ! v4l2convert output-io-mode=dmabuf-import capture-io-mode=dmabuf \
  ! v4l2h264enc output-io-mode=dmabuf-import \
      extra-controls="controls,video_bitrate={bitrate_bps},video_bitrate_mode=0,h264_i_frame_period={gop},repeat_sequence_header=1" \
  ! "video/x-h264,profile=constrained-baseline,level=(string){level}" \
  ! h264parse config-interval=-1 \
  ! rtph264pay pt=96 mtu=1200 config-interval=-1 aggregate-mode=zero-latency timestamp-offset=0 \
  ! funnel name=f ! rtpstreampay ! fdsink fd=1 sync=false \
  {audio branch exactly as in §3}
```

### Template B: software JPEG decode (recommended for camera 2 / USB 1.1 cameras)

Only the decode stage is different:

```
... ! image/jpeg,width={width},height={height},framerate={fps}/1 ! jpegdec \
    ! v4l2h264enc extra-controls="controls,video_bitrate={bitrate_bps},video_bitrate_mode=0,h264_i_frame_period={gop},repeat_sequence_header=1" \
    ! "video/x-h264,profile=constrained-baseline,level=(string){level}" ! h264parse config-interval=-1 ! ...
```

For both templates, the video-only variant is the same string without the audio branch.

Parameters:
- `{gop}` = `2*{fps}`, giving one IDR every 2 s. Every I-frame was an IDR and carried SPS/PPS.
- `{level}` = `3.1` up to 1280x720@30, and `4` for 1920x1080. **Use `level=(string)3.1`**: an
  unquoted `level=3.1` is parsed as a double and the link fails with "could not link
  v4l2h264enc0 to h264parse0".
- `{bitrate_bps}` is in bit/s, range 25000–25000000 in steps of 25000.

### What each part does

- **`video_bitrate_mode=0` (VBR) is required.** With CBR (`=1`), the Pi's encoder drops to
  about 11 fps at 720p (measured 11.2 fps and 754 kbit/s for a 2000 kbit/s target).
  The VBR output still tracks the target closely (table below).
- The profile is set through the downstream caps. `v4l2h264enc` maps the caps to the
  `h264_profile`/`h264_level` controls. The default is High, which is not what we want.
  Measured SPS: `42c01f` (CB 3.1) and `42c028` (CB 4.0).
- `repeat_sequence_header=1` makes the encoder repeat SPS/PPS itself, and h264parse /
  rtph264pay add them anyway.
- **Zero-copy (`capture-io-mode=dmabuf` / `output-io-mode=dmabuf-import`)** decides the CPU
  cost. If the decoder's output is copied to the encoder, the copy runs in the decoder's src
  thread: 40 % of a core at 720p and 90 % at 1080p. With dmabuf it is 8–20 %. Other io-mode
  combinations fail (mmap → dmabuf-import gives "Failed to process frame"; dmabuf-import
  into the decoder gives a not-negotiated error).
- **`v4l2convert` (ISP) between decoder and encoder is required for heights that aren't a
  multiple of 16, such as 1080.** Without it, 1080p zero-copy produces a green/yellow band
  of corrupted chroma at the top of the picture: the decoder buffer is 1088 lines and the
  encoder assumes 1080. With the ISP in the chain the picture is clean at all sizes, and the
  ISP adds about 0–1 % CPU. It is harmless at 720p and 480p, so use one template for all
  sizes.
- **Hardware JPEG decode is fragile with bad frames.** When a corrupt or truncated MJPEG
  frame arrives, `bcm2835-codec-decode` returns an empty LAST buffer. `v4l2jpegdec` treats
  that as EOS, and **the pipeline then sits there silently**: no error, no exit, no more
  output. Camera 2 (USB 1.1 behind the Pi 3's dwc_otg/FIQ) delivers broken frames often:
  JPEG padding after EOI, and sometimes two frames merged into one buffer. In every test the
  HW decoder stalled within 1–60 s. Things that did not help: `jpegparse` (it fails
  intermittently with "Invalid data") and `uvcvideo nodrop=0`.
  Software `jpegdec` handles these frames fine and costs only 8 % of a core at 640x480 @ 11 fps.
  Camera 1 ran 10 minutes with the HW decoder (plus about 20 shorter runs) without a stall,
  but the agent still needs the stdout watchdog (§7).
- Recommended rule for the agent: **HW decode when width×height > 640×480, otherwise
  `jpegdec`**. If a camera keeps stalling with HW decode, fall back to `jpegdec`.
  Software decode at 720p costs 36 % of a core (camera 1), which is acceptable for one camera.

### Measured (30 s runs unless noted; CPU = gst-launch process, % of one core; Pi 3 has 4)

| Camera / setting | Decode | Target | Real bitrate | Real fps | CPU | First IDR |
|---|---|---|---|---|---|---|
| cam1 1280x720@30 | HW dmabuf | 1000 k | 960 k | 28.4 | 10.8 % | 1.45 s |
| cam1 1280x720@30 | HW dmabuf | 2000 k | 1922 k | 28.4 | 12.0 % | 1.38 s |
| cam1 1280x720@30, 10 min + audio (opus cx10) | HW dmabuf | 1500 k | 1494 k | 29.5 | 31 % | 1.49 s |
| cam1 1280x720@30 | HW + v4l2convert | 1000 k | 942 k | 27.9 | 10.4–11.2 % | 1.41 s |
| cam1 640x480@30 | HW dmabuf | 1000 k | 970 k | 28.7 | 8.4 % | 1.13 s |
| cam1 640x480@30 | HW + v4l2convert | 2000 k | 1813 k | – | 7.6 % | – |
| cam1 1920x1080@30 | HW + v4l2convert | 3000 k | 2870 k | 28.2 | 20–27 % | 1.59 s |
| cam1 1920x1080@30 | HW dmabuf, no convert | 4000 k | 3798 k | 28.0 | 22.6 % | 1.44 s (**corrupt chroma band**) |
| cam1 1920x1080@30 | HW, copy (no dmabuf) | 2000 k | 2000 k | – | 90 % | – |
| cam1 1280x720@30 | jpegdec (SW) | 2000 k | 1955 k | – | 36 % | – |
| cam1 640x480@30 | jpegdec (SW) | 2000 k | 1995 k | – | 27 % | – |
| cam2 640x480 (~10 fps real) | jpegdec (SW) | 1000 k | 330–360 k | 9.7–10.4 | 8.4 % | 0.53–0.66 s |
| cam2 640x480, 10 min + audio (opus cx10) | jpegdec (SW) | 1000 k | 350 k | 10.4 | 24 % | 0.66 s |
| cam2 640x480 | HW | – | – | **stalls after 1–60 s** | – | – |

- **The Pi 3 encoder keeps up with 1080p30**: 28 fps, which is what the camera delivers.
- Camera 2 says 30 fps but delivers about 10–11 fps at 640x480 (7.5–11 under bus load).
  The encoder budgets bits per frame from the caps frame rate, so the real bitrate is about
  target × real_fps / caps_fps (about 1/3). It is always **below** the target, so it never
  breaks `max_kbps`.
- 1080p over hwenc decoded correctly on the laptop (1920x1080), as did 720p and 480p.

## 5. Audio and A/V sync

Use these alsasrc settings for every mic:

```
alsasrc device={audio_device} provide-clock=false slave-method=re-timestamp
  ! audio/x-raw,rate={audio_rate},channels=1 ! audiorate ! audioconvert ! audioresample
  ! audio/x-raw,rate=48000 ! opusenc bitrate=32000 complexity=5 ! rtpopuspay pt=111 timestamp-offset=0
```

- `{audio_rate}`: camera 1 → 48000 (no resampling). Camera 2 → **32000**, the only rate it
  supports. In general, choose 48000 if the card supports it, otherwise the highest rate it
  does.
- `provide-clock=false` makes the system monotonic clock the pipeline clock, instead of a
  clock driven by the USB mic. `slave-method=re-timestamp` timestamps audio buffers by system
  time. `audiorate` fills gaps with silence and drops overlaps, so the Opus RTP timestamps
  stay continuous and track wall-clock time.
- **Unmute the mic before starting.** Camera 1's `Mic Capture Switch` was **off** after
  boot. Run: `amixer -c {card} cset name='Mic Capture Switch' on`, where `{card}` is the
  ALSA card id (`Camera`, `UVC`). This is harmless if the card has no such control; ignore
  the error.
- Opus CPU on the Pi 3 (audio branch alone): complexity 10 (default) 15 %, **complexity 5
  8.8 %**, 16 kHz + cx5 7.9 %. `audio-type=voice` costs more (13.9 %).

### Drift measurements (10 min each, `rtpstat.py` on the Pi)

drift(pt) = (rtp_ts − first_ts)/clock − (arrival − first_arrival); av = drift(96) − drift(111).

| Config | Camera / mode | av over 10 min | Wall drift after 600 s (video / audio) |
|---|---|---|---|
| **A** default alsasrc (it provides the pipeline clock) | cam1 passthrough 720p | +0.002…+0.006 s | −0.018 / −0.022 s |
| A | cam2 jpegdec+hwenc (cam1 running at the same time) | +0.03…+0.12 s | **−139.5 / −139.5 s** ❌ |
| **B** `provide-clock=false slave-method=re-timestamp` + `audiorate` | cam1 passthrough 720p | −0.023…+0.009 s (one +0.03 s step at about 300 s) | +0.001 / +0.019 s |
| B | cam2 jpegdec+hwenc 640x480 (cam1 running at the same time) | +0.032…+0.078 s (follows 10 fps frame jitter) | +0.04 / +0.003 s |
| B | cam1 HW-dec+hwenc 720p | +0.060…+0.069 s, constant | +0.063 / −0.006 s |

How to read these numbers:
- In config A, camera 2's mic **loses samples** whenever its video is streaming (dmesg shows
  `FIQ reported NYET / FIQ timed out. Data may have been lost`). Measured with arecord:
  32 000 samples/s when the mic runs alone, 29 300/s with its own video, and about 24 600/s
  with camera 1 streaming as well. The pipeline clock is driven by the mic, so all
  timestamps, video included, fell behind real time by 23 %. A and V stayed in sync with
  each other, but the stream ran 23 % slow compared to the wall clock: a browser playout
  buffer would drain or bloat, and this is very likely the v1 failure.
  Config B fixes this. Lost audio is replaced by silence, so camera 2's audio has
  **dropouts**. That is a hardware/USB limit of this camera on the Pi 3.
- A constant av offset of about 0.06 s (hwenc) comes from different first-packet latency
  (encoder start-up). It doesn't grow.
- In config B nothing grows: |av| < 0.08 s for both cameras over 10 minutes. The server's
  1.0 s threshold has a large margin.

## 6. Native H.264 bitrate control (camera 1, 32e4:9422): UVC extension units

The standard V4L2 controls have no bitrate or GOP setting (`v4l2-ctl -l` lists only the
image/exposure controls). `lsusb -v -d 32e4:9422` shows two vendor extension units:

| Unit | GUID | bNumControls | Selectors that answer GET_INFO/GET_LEN |
|---|---|---|---|
| 3 | `{28f03370-6311-4a2e-ba2c-6890eb334016}` | 24 | 1 (len 4), 2 (len 8), 3, 4, 5 (len 11) |
| 4 | `{dddf7394-973e-4727-bed9-04ed6426dc67}` | 8 | 2, 3, 4, 6, 8 (len 11), 5 (len 24) |

Read-only dump (`python3 tools/uvcxu.py /dev/v4l/by-id/...-video-index0 dump 3 4`):

```
unit 3 sel  1 len=4  cur=00000100
unit 3 sel  2 len=8  cur=0100121215160500
unit 3 sel  3 len=11 cur=0000f012ffffffff000000
unit 3 sel  4 len=11 cur=000000ffffffffffffffff
unit 3 sel  5 len=11 cur=390401ffffffffffffffff
unit 4 sel  2/3/4/6/8 len=11 cur=0b0001ffffffffffffffff
unit 4 sel  5 len=24 cur=180001ffffffffffffffff0000000000000000d007004006
```

These are the GUIDs of the **Sonix SN9C29x "RERVISION" SYS (unit 3) and USR (unit 4)
XUs**, the ones driven by Sonix's `H264_UVC_TestAP` (`sonix_xu_ctrls.c`), which ELP-type
cameras also use. Their selector layout fits the USR unit: sel 2 = H264_CTRL, 3 = MJPG_CTRL,
4 = OSD, 5 = MOTION_DETECTION (24 bytes), 6 = IMG_SETTING, 8 = GPIO.

In TestAP, bitrate is changed through USR selector 2 with a two-step "select sub-command,
then read or write" protocol:

```
# NOT VERIFIED on this camera. Recalled from the Sonix TestAP source.
uvcxu.py DEV set 4 2 9a02            # select sub-command 0x02 = H.264 bitrate
uvcxu.py DEV get 4 2                 # read: bytes[0..2] = bitrate (bit/s), big-endian 24-bit
uvcxu.py DEV set 4 2 0f4240          # write 1 000 000 bit/s (0x0F4240), RAM only
# 9a01 = rate-control mode (CBR/VBR), other 9aXX = GOP/QP etc.
```

**Status: not tested.** Running the SET_CUR sub-command selector (even the read-back
sequence starts with a SET) was blocked by this session's permission policy, so I only did
the read-only GET_* queries. Points to know before trying it:
- The SYS unit (3) has flash and ASIC read/write commands in the Sonix firmware. **Never SET on unit 3.**
- The USR H264_CTRL sub-commands are documented by Sonix as runtime-only (RAM). To try:
  stream the H.264 node, run the three commands above with the agreed bitrate, and watch
  `rtpstat.py` kbps. The value is lost on replug, so the agent would have to apply it
  before every `start`, while the device is open but before STREAMON, or right after.
- If it works, the agent can apply `max_kbps` to passthrough mode. Until then, assume the
  camera **always sends about 6.5 Mbit/s** and use `mjpeg-hwenc` when `max_kbps` < about 7000.

## 7. Recommendations for the Go agent (supervisor)

**Starting**
- Always run `gst-launch-1.0 -q ...` (no `-e`). Pass the pipeline as argv tokens, not through
  a shell. Caps with `(string)` must be one argv element.
- Before start: run `amixer ... 'Mic Capture Switch' on` for the mic, and confirm that the
  `/dev/v4l/by-id` link exists.
- No V4L2 controls need setting beforehand. All encoder settings go in `extra-controls`.
- Start-up, measured from launch to the first IDR on stdout: **0.5–0.7 s** (camera 2, SW
  decode), **1.1–1.6 s** (camera 1, any mode). Launched over ssh it was 2.2–2.6 s. Allow
  about 5 s before you declare the start failed.

**Stopping**
- **`-e` + SIGINT hangs forever with the HW JPEG decoder**: the EOS drain never completes.
  This happened in 3 out of 3 attempts (SIGKILL needed, rc 137). Without `-e`, SIGINT exits
  in about 0.1 s with rc 0, and SIGTERM in about 0.1 s with rc 143. EOS has no value for a
  live RTP stream.
- Stop sequence: SIGINT, wait 2 s, then SIGKILL. After a SIGKILL the codec device is freed
  (the next start worked right away every time).

**Detecting failure**
- **Most important: watch stdout bytes.** The HW-decoder stall (§4) produces **no stderr
  output and no exit**. If no stdout data arrives for 3 s after the first packet (or 5 s at
  start), kill and restart. At 30 fps there are packets every 33 ms, and audio sends one
  every 20 ms.
- Exit codes: 0 = clean stop after SIGINT. **1 = error**, for example a missing or busy
  device, a caps or link failure, or an unplug. 143/137 = killed.
- Error messages on stderr start with `ERROR: from element /GstPipeline:pipeline0/...`.
  The ones seen:
  - Unplugged camera (simulated with USB `authorized=0`): `GstV4l2Src:v4l2src0: Could not read
    from resource.` / `poll error 1: Invalid argument (22)`, then exit rc 1 within about 0.1 s.
  - Same camera already in use: `Device '/dev/v4l/by-id/...' is busy` (rc 1).
  - Bad caps or resolution: `WARNING: erroneous pipeline: could not link ...` or
    `streaming stopped, reason not-negotiated (-4)` (rc 1).
  - HW encoder/decoder wrong io-mode: `v4l2h264enc0: Failed to process frame ... Maybe be due
    to not enough memory or failing driver`.
  - Mic busy or missing: `alsasrc ... Could not open audio device for recording` (expected; not
    triggered in tests).
- Harmless stderr noise: `v4l2bufferpool ... Uncertain or not enough buffers, enabling copy
  threshold` (only with GST_DEBUG on).
- Restart strategy: 1st failure → retry. With HW decode, after 2 stalls in 60 s, switch that
  camera to `jpegdec`. If the mic branch keeps failing, start video-only and report it.

**Other things to know**
- Camera 2 (USB 1.1) on the Pi 3: if camera 1 is streaming at the same time, camera 2's
  isochronous transfers lose data: dropped audio samples and broken JPEG frames. This is a
  Pi 3 dwc_otg limitation, and Pi 4 (xHCI) should be better. Prefer 320x240 for camera 2 if
  its audio matters.
- In one run of camera 2 (1 out of about 6), the mic delivered digital silence the whole
  time. It recovered on the next start. Root cause unknown.
- The `uvcvideo` module parameter `nodrop` defaults to 1 on this kernel (corrupt frames are
  passed on). Changing it didn't fix the HW-decoder stall, so leave it alone.
- The Pi 3 has a single hardware encoder, and it handled one 1080p30 stream at about 20 % CPU.
  Two cameras in hwenc at the same time were not tested. Camera 2 jpegdec+hwenc plus camera 1
  passthrough ran together fine for 10 minutes.
- For lip-sync in browsers, the server must send RTCP SRs, because the agent sends none.
  With `timestamp-offset=0` on both payloaders, rtp_ts/clock is the pipeline running time for
  both streams, so the server can derive a common NTP mapping from them.

## 8. Tools (`tools/`)

- `rtpstat.py`: reads RFC 4571 on stdin. Prints per-interval fps, kbps and drift per pt, the
  first-IDR time, IDR+SPS checks, sequence gaps, max packet size and A/V drift. `--dump FILE`
  saves the stream.
- `runtest.sh`: runs a pipeline on the Pi into rtpstat for N seconds and samples CPU with
  pidstat.
- `cpuprobe.sh`: quick CPU/output-rate probe of a pipeline.
- `uvcxu.py`: UVC XU query tool (`dump`/`get` are read-only; `set` writes runtime controls).

Decoding a stream on a machine without h264parse (laptop):
```
ssh pi 'gst-launch-1.0 -q ...' > s.rtp     # or pipe directly
# split by pt (or use only video), then:
gst-launch-1.0 filesrc location=v.rtp ! 'application/x-rtp-stream,media=video,clock-rate=90000,encoding-name=H264,payload=96' \
  ! rtpstreamdepay ! rtph264depay ! avdec_h264 ! videoconvert ! jpegenc ! multifilesink location=f_%04d.jpg
gst-launch-1.0 filesrc location=a.rtp ! 'application/x-rtp-stream,media=audio,clock-rate=48000,encoding-name=OPUS,payload=111' \
  ! rtpstreamdepay ! rtpopusdepay ! opusdec ! audioconvert ! wavenc ! filesink location=a.wav
```
