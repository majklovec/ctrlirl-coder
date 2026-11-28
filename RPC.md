# Runtime Remote Control (JSON-RPC 2.0 over the Shared HTTP API)

The runtime can be controlled live: change layer parameters, add/remove
layers, add/remove/update filters, and switch or edit scenes — without
restarting the process. The control plane is a **JSON-RPC 2.0** endpoint at
**`POST /rpc`** on the **shared HTTP server**, which also serves the
**`GET /metrics`** endpoint (Prometheus text exposition format) on the same
listener.

Enable it with the `--api-addr host:port` flag or `[control] addr` in the
TOML config (the flag wins). With neither set, no listener is created and
the control plane is disabled.

```
$ ./ctrlirl-coder --config ctrlirl_coder.conf.toml --scene scene.json \
    --pipeline pipeline.st --api-addr 127.0.0.1:8899
$ curl -s -X POST 127.0.0.1:8899/rpc -H 'Content-Type: application/json' \
      -d '{"jsonrpc":"2.0","id":1,"method":"scene.get"}'
```

## Semantics

- Every mutation validates the **resulting scene** (same rules as
  `scene.Parse`/`Validate`: ids, z-orders, filter params). Invalid input →
  JSON-RPC error, pipeline untouched.
- Chain-affecting changes (type, source, text, text-options, filters) rebuild
  the layer's element chain (remove + add); plain pad-property changes update
  in place.
- `scene.switch` / `scene.load` replace the whole current scene: layers not
  in the target are removed, new ones added, common ones diffed.
  `scene.load` with `"switch": false` only registers the scene (result
  `"registered"`) — the running scene is untouched.
- Removing a live layer first freezes the pipeline to READY (back to
  PLAYING after the remove): a live source (uridecodebin/HLS) can block
  forever in `gst_element_set_state(NULL)` draining its network read, which
  would wedge the command goroutine and hang SIGINT shutdown.
- **Atomic-ish apply with rollback**: ops are applied one at a time in
  remove → update → add order. If any op fails, the ops already applied are
  inverted (in reverse) and the pre-mutation scene is restored. The response
  reports the failure.
- `scene.save` encodes the current (possibly RPC-modified) scene as JSON.

## Methods

### System

| Method          | Params | Result                                                   |
| --------------- | ------ | -------------------------------------------------------- |
| `system.ping`   | —      | `"pong"`                                                 |
| `system.status` | —      | `{scene, layers: [{id, type, zorder, filters}], scenes}` |

### Scene

| Method         | Params                                              | Result                                                                                                                                                                                                         |
| -------------- | --------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `scene.get`    | —                                                   | current scene JSON (as in `scene.example.json`)                                                                                                                                                                |
| `scene.names`  | —                                                   | `{scenes: ["main", ...]}` named scenes                                                                                                                                                                         |
| `scene.load`   | `{name}` or `{json: <scene object>, switch?: bool}` | `{applied: [...]}` apply an inline scene (full switch), or `{name}` for a named one. `"switch": false` registers the scene for later `scene.switch` without touching the running scene (result `"registered"`) |
| `scene.switch` | `{name}`                                            | `{applied: [...]}` switch to a previously loaded/registered scene                                                                                                                                              |
| `scene.save`   | `{path, name?}`                                     | `{path}` write the current (or named) scene to a file                                                                                                                                                          |

### Layer

| Method         | Params                                                                | Result                                                                                      |
| -------------- | --------------------------------------------------------------------- | ------------------------------------------------------------------------------------------- |
| `layer.add`    | `{layer: <layer object>}`                                             | `{applied: [...]}`                                                                          |
| `layer.remove` | `{id}`                                                                | `{applied: [...]}`                                                                          |
| `layer.set`    | `{id, source?, type?, text?, properties?, "text-options"?, filters?}` | `{applied: [...]}` present fields replace the existing layer's fields (omitted → unchanged) |

A layer object matches the scene schema:

```json
{
    "id": "cam0",
    "type": "video",
    "source": "/dev/video0",
    "properties": {
        "zorder": 1,
        "xpos": 0,
        "ypos": 0,
        "width": 640,
        "height": 360
    },
    "filters": [
        { "kind": "videocrop", "params": { "top": 100, "bottom": 100 } },
        { "kind": "volume", "params": { "volume": 0.8 } }
    ]
}
```

### Filter

| Method          | Params                        | Result                                     |
| --------------- | ----------------------------- | ------------------------------------------ |
| `filter.add`    | `{id, index?, filter}`        | insert (index = position, default: append) |
| `filter.remove` | `{id, index}`                 | remove by position                         |
| `filter.set`    | `{id, index, kind?, params?}` | update kind/params in place                |

## Wire format

JSON-RPC 2.0 over HTTP. `POST /rpc` takes one request object (or a 2.0
batch: a JSON array of requests) as the body; the response mirrors the
shape (object → object, array → array). A batch where every request is a
notification (no `id`) returns `204 No Content` with no body.

```json
{
    "jsonrpc": "2.0",
    "id": 42,
    "method": "layer.set",
    "params": { "id": "cam0", "properties": { "zorder": 3 } }
}
```

Response:

```json
{ "jsonrpc": "2.0", "id": 42, "result": { "applied": ["update:cam0"] } }
```

`applied` lists what the edit did to the pipeline, as `<op>:<layer id>` in
the order it was applied: `remove`, `update` (compositor-pad properties),
`update-filter` (an in-place filter change), `add`. One edit can produce
several entries — e.g. `["update:alerts", "update-filter:alerts"]` when it
moves a layer and changes its volume, or `["remove:cam0", "add:cam0"]` when
the change needs the layer's chain rebuilt. An empty list means nothing
changed.

Errors use standard codes: `-32700` parse error, `-32600` invalid request,
`-32601` method not found, `-32602` invalid params. Application errors
(validation, pipeline apply failure) use `-32000` with a descriptive message.
Bodies above 4 MiB are rejected with `413`.

## Examples

```sh
API=127.0.0.1:8899
req() { curl -s -X POST "http://$API/rpc" -H 'Content-Type: application/json' \
         -d "{\"jsonrpc\":\"2.0\",\"id\":$1,\"method\":\"$2\",\"params\":$3}"; }

req 1 scene.get
req 2 layer.add '{"layer":{"id":"logo","type":"image","source":"f:/tmp/logo.png","properties":{"zorder":9,"xpos":100,"ypos":100,"width":200,"height":200}}}'
req 3 filter.add '{"id":"logo","filter":{"kind":"videobalance","params":{"brightness":0.1}}}'
req 4 filter.set '{"id":"logo","index":0,"params":{"brightness":0.2}}'
req 5 layer.remove '{"id":"logo"}'
req 6 scene.switch '{"name":"main"}'
req 7 scene.save '{"path":"/tmp/scene-new.json"}'

# metrics on the same listener (Prometheus text exposition format)
curl -s "http://$API/metrics"
```
