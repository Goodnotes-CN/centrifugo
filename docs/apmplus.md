# APMPlus 接入说明

本文档说明如何通过火山引擎 APMPlus 对 Centrifugo 进行全链路可观测接入，覆盖 **Traces、Metrics、Logs** 三类信号。

> APMPlus 使用标准 OpenTelemetry 协议（OTLP）接收数据，无需引入私有 SDK。
> 官方参考：[Go 服务接入 OpenTelemetry](https://www.volcengine.com/docs/6431/97735?lang=zh)

---

## 数据流向

```
Centrifugo
  ├── Traces  → OTel TracerProvider → OTLP HTTP → APMPlus
  ├── Metrics → OTel MeterProvider  → OTLP HTTP → APMPlus
  │     ├── Go runtime（goroutine、GC、内存）
  │     └── Prometheus bridge（centrifugo_* 全部业务指标）
  └── Logs    → OTel LoggerProvider → OTLP HTTP → APMPlus
        └── zerolog bridge（保留本地 stderr 输出不变）
```

---

## 环境变量配置

以下变量注入到 K8s Deployment 中。

### APMPlus 连接（Secret）

| 变量 | 说明 |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | APMPlus OTLP 接入地址，例如 `https://apmplus.cn-beijing.volces.com` |
| `OTEL_EXPORTER_OTLP_HEADERS` | 认证 Header，格式：`Authentication=<base64-token>` |

### 服务标识（ConfigMap）

| 变量 | 说明 | 默认值 |
|---|---|---|
| `OTEL_SERVICE_NAME` | APMPlus 中显示的服务名 | `centrifugo` |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | 传输协议 | `http/protobuf` |
| `OTEL_GO_X_DEPRECATED_RUNTIME_METRICS` | 禁用已废弃的 runtime 指标，避免重复 | `false` |

### Centrifugo OTel 开关（ConfigMap）

| 变量 | 说明 |
|---|---|
| `CENTRIFUGO_OPENTELEMETRY_ENABLED` | 总开关，必须为 `true` |
| `CENTRIFUGO_OPENTELEMETRY_API` | 为 HTTP/gRPC API 每个命令创建 trace span |
| `CENTRIFUGO_OPENTELEMETRY_CONSUMING` | 为 Kafka/SQS 等 Consumer 消费消息创建 trace span |
| `CENTRIFUGO_OPENTELEMETRY_METRICS` | 启用 OTLP Metrics 上报（Prometheus bridge + runtime） |
| `CENTRIFUGO_OPENTELEMETRY_LOGS` | 启用 OTLP Logs 上报（zerolog bridge） |

### 完整示例

```yaml
# configmap.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: centrifugo-otel
data:
  OTEL_SERVICE_NAME: "centrifugo"
  OTEL_EXPORTER_OTLP_PROTOCOL: "http/protobuf"
  OTEL_GO_X_DEPRECATED_RUNTIME_METRICS: "false"
  CENTRIFUGO_OPENTELEMETRY_ENABLED: "true"
  CENTRIFUGO_OPENTELEMETRY_API: "true"
  CENTRIFUGO_OPENTELEMETRY_CONSUMING: "true"
  CENTRIFUGO_OPENTELEMETRY_METRICS: "true"
  CENTRIFUGO_OPENTELEMETRY_LOGS: "true"
---
# secret.yaml
apiVersion: v1
kind: Secret
metadata:
  name: centrifugo-otel-secret
stringData:
  OTEL_EXPORTER_OTLP_ENDPOINT: "https://apmplus.cn-beijing.volces.com"
  OTEL_EXPORTER_OTLP_HEADERS: "Authentication=<base64-token>"
```

---

## 上报的指标清单

Metrics 开启后，以下指标全部推送至 APMPlus：

### Go Runtime（来自 `runtime.NewProducer()`）
- goroutine 数量、GC 次数/耗时、堆内存使用、调度延迟直方图等

### Centrifugo 业务指标（来自 Prometheus bridge）

| 前缀 | 内容 |
|---|---|
| `centrifugo_proxy_*` | proxy 调用耗时、错误数、在途请求数 |
| `centrifugo_api_*` | API 命令耗时、错误数、RPC 耗时 |
| `centrifugo_consumers_*` | Consumer 消费消息数、错误数 |
| `centrifugo_node_*` | 连接数限制触发次数、HTTP 请求总数 |
| `centrifuge_*` | centrifuge 底层库：连接数、订阅数、消息数等 |

---

## 注意事项

1. **token 格式**：`OTEL_EXPORTER_OTLP_HEADERS` 的值需为 `Authentication=<base64-token>`，token 从 APMPlus 控制台获取。
2. **Logs 输出**：启用 Logs 后，zerolog 仍同时输出到 stderr（本地日志不受影响），日志会额外推送一份到 APMPlus。
3. **Metrics 采集间隔**：默认 60 秒上报一次（OTel SDK 默认值），可通过 `OTEL_METRIC_EXPORT_INTERVAL=30000`（毫秒）调整。
4. **关闭废弃指标**：`OTEL_GO_X_DEPRECATED_RUNTIME_METRICS=false` 必须设置，否则 runtime 指标会被重复采集两次。
