# Gateway smoke profile template (direct proxy ENH)

Use this template with `helianthus-ebusgateway` smoke (`EBUS_SMOKE=1`) when the gateway must connect directly to the adapter-proxy ENH endpoint.
Keep source-address separation from `ebusd` by using a gateway-only source (example below uses `0xF0` while `ebusd` remains on `0x31`).

```yaml
enh:
  type: tcp
  host: "127.0.0.1"
  port: 19001
  timeout_sec: 10

smoke:
  profile: enh
  source_address: 0xF0
  scan_timeout_sec: 8
  method_timeout_sec: 10
  report_json_output: "artifacts/smoke-report.enh.json"
```
