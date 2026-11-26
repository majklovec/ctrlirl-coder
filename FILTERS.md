# Filters

Per-layer media filters for the scene descriptor (`"filters": [...]` in the
scene JSON). Video filters splice into the layer chain between the source and
its `videoconvert` tail, in scene order; audio filters sit in the browser
layer's audio chain between the `audioconvert` and the AAC encoder (browser
layers are the only layers that carry audio). Kinds and params are validated
on scene load — unknown kinds/params are rejected with an error. A filter
change on `SIGHUP` tear-downs and re-attaches the layer (running chains
cannot be spliced).

Entry shape: `{ "kind": "<name>", "params": { … } }` — `params` is optional
(omitted params take the defaults below).

**Chain layout.** Video filters are inserted between the source and the
tail `videoconvert` of the layer chain. A `videoconvert` element
(`<id>-fconv`) is placed in front of the filter list whenever the layer
declares any video filter: decoders (decodebin, srtsrc, jpegdec) emit
I420/Y420-family formats, and some filters — notably `rotate` — refuse
them. Without the preconvert the link fails, the layer's compositor pad
never produces a buffer, and the compositor (which needs every pad) holds
its whole output — the entire scene goes black.

## 1. Video Filters

These elements modify the visual content, dimensions, or format of video
frames.

| kind | params (default) | element |
| :--- | :--- | :--- |
| `videoscale` | `width` (0), `height` (0) — at least one required | `videoscale` + trailing `capsfilter` carrying the target size (the element only conforms to the caps it is given) |
| `videocrop` | `top` (0), `bottom` (0), `left` (0), `right` (0) | `videocrop` |
| `videobalance` | `brightness` (0, ±1), `contrast` (1, ≥0), `hue` (0, ±1), `saturation` (1, ≥0) | `videobalance` |
| `videobox` | `top` (0), `bottom` (0), `left` (0), `right` (0) | `videobox` — borders/padding for picture-in-picture or aspect-ratio work |
| `videoflip` | `direction` ("horizontal"; `none`\|`horizontal`\|`vertical`) | `videoflip` — maps to the `video-direction` element enum |
| `videorotate` | `angle` (0, degrees) | `rotate` (plugins-bad; the element stores radians — degrees are converted). `rotate` accepts RGB-family formats only — the chain carries a `videoconvert` in front of the filter list (see chain layout). A `capsfilter` pins `rotate`'s output to **BGRA** (an alpha format) right after it: `rotate`'s `geometrictransform` base clears the output frame with `memset(0)` before mapping, which is transparent only in an alpha format — in an opaque format the uncovered corners come out solid black. A trailing `videoconvert` follows when a later filter in the list needs the format back (YUV-only filters such as `effectv` cannot keep the alpha, so corners stay black in that order) |
| `effectv` | booleans: `aging-tv`, `edge-tv`, `dice-tv`, `vcr-tv`, `vhs-tv`, `rainbow-tv`, `shred-tv`, `mpeg1-tv`, `mpeg2-tv`, `scanline-tv` (at least one enabled) | `effectv` — real-time visual effects (edgetv = edge detection, agingtv = old film look, dicetv, …). **Availability caveat:** the element is registered in some GStreamer builds, but stock Debian (bookworm/trixie) does not register it in the plugin registry — on such hosts a scene using it fails at chain build with a clear `effectv element factory not available` error |

## 2. Audio Filters

These elements alter audio waveforms, channel layouts, or sample rates.
Accepted on `browser` layers only.

| kind | params (default) | element |
| :--- | :--- | :--- |
| `volume` | `volume` (1.0), `mute` (false) | `volume` — gain or full mute |
| `equalizer` | `bands` (3; 3\|4\|5\|10), band gains `band0`…`bandN-1` (0, -24 to +12 dB) | `equalizer-{3,4,5,10}bands` — shapes specific frequency bands |
| `audiodynamic` | `mode` (0; 0 compressor, 1 expander), `characteristics` (0; 0 hard-knee, 1 soft-knee), `ratio` (1, ≥0), `threshold` (0, 0–1 normalized) | `audiodynamic` — dynamic-range compression/expansion |
| `pitch` | `pitch` (1.0), `tempo` (1.0), `rate` (1.0) — each 0.1–10 | `pitch` — tone/tempo shift (raise one of them) |

## Example

```json
{
  "id": "cam",
  "type": "browser",
  "source": "https://example.com",
  "properties": { "zorder": 5, "width": 640, "height": 360 },
  "filters": [
    { "kind": "videocrop", "params": { "top": 24, "bottom": 24 } },
    { "kind": "videobalance", "params": { "brightness": 0.05 } },
    { "kind": "effectv", "params": { "edge-tv": true } },
    { "kind": "volume", "params": { "volume": 0.8 } },
    { "kind": "equalizer", "params": { "bands": 3, "band0": 1.5 } }
  ]
}
```
