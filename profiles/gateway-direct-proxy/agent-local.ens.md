# Gateway smoke profile template (direct proxy ENS)

Use this template with `helianthus-ebusgateway` smoke (`EBUS_SMOKE=1`) when the gateway must connect directly to the adapter-proxy ENS endpoint.
Keep source-address separation from `ebusd` by using a gateway-only source (example below uses `0xF1` while `ebusd` remains on `0x31`).

```yaml
enh:
  type: tcp
  host: "127.0.0.1"
  port: 19002
  timeout_sec: 10

smoke:
  profile: ens
  source_address: 0xF1
  scan_timeout_sec: 8
  method_timeout_sec: 10
  report_json_output: "artifacts/smoke-report.ens.json"
```
