TINYGO ?= tinygo
GO ?= go
BUILD := out
COLLECTOR_IMAGE := otel/opentelemetry-collector-contrib:0.160.0
# The host may already run something on 4318 (a local Alloy or collector), so
# the demo publishes on 4319 by default. Only the host-side port changes; the
# container and the device still speak the standard 4318.
COLLECTOR_PORT ?= 4319

# Override on the command line. Credentials are never written to a file.
#   make flash SSID=mynet PASSWORD=secret ENDPOINT=http://192.168.1.10:4318/v1/metrics
SSID ?=
PASSWORD ?=
ENDPOINT ?=
# esp32s3-generic rather than xiao-esp32s3: the two produce byte-identical
# binaries for this program, but the xiao target defines XIAO pin numbers
# (SDA=GPIO5, SCL=GPIO6) that are wrong for other ESP32-S3 boards. On
# M5Stack CoreS3 the internal I2C bus is SDA=GPIO12/SCL=GPIO11 and Grove
# Port A is SDA=GPIO2/SCL=GPIO1. The generic target leaves every pin as
# NoPin, so a wrong pin fails to compile instead of failing silently.
TARGET ?= esp32s3-generic
# Build tags, space separated. Two independent choices:
#   socket   -- hand-written HTTP client instead of net/http
#   otlpjson -- encoding/json instead of hand-written binary protobuf
# The default is the combination with the least custom code: no hand-written
# encoder, OTLP spoken with the standard library alone.
# Empty means net/http + protobuf.
TAGS ?= socket otlpjson

# encoding/json marshals the OTLP message tree recursively and overflows the
# 8 KB default goroutine stack on this target. Measured on M5Stack CoreS3:
# 12 KB overflows, 16 KB works. The protobuf path does not need this, but
# setting it unconditionally keeps one flag for both encodings.
STACK_SIZE ?= 16KB

LDFLAGS = -X main.ssid=$(SSID) -X main.password=$(PASSWORD) \
	-X main.endpoint=$(ENDPOINT) -X main.target=$(TARGET)

.PHONY: test vet build build-all flash monitor collector collector-stop \
        stack stack-stop hostsim encode sizes clean

test:
	$(GO) test ./...
	$(GO) test -tags socket ./...

vet:
	$(GO) vet ./otlpmini ./cmd/hostsim
	$(GO) vet -tags socket ./otlpmini

$(BUILD):
	mkdir -p $(BUILD)

build: $(BUILD)
	$(TINYGO) build -target=$(TARGET) -tags "$(TAGS)" -stack-size=$(STACK_SIZE) \
		-o $(BUILD)/device-$(TARGET)$(if $(TAGS),-$(subst $() ,-,$(TAGS)),).bin ./cmd/device

# SIZE_LDFLAGS fills in every -ldflags variable that gates a code path, so
# that a size measurement describes the program that actually runs.
#
# Two separate paths get eliminated when these are empty, and missing either
# one understates the binary by roughly 6x:
#   endpoint == ""  -> main() calls fatal() before the export loop
#   ssid == ""      -> NetConnect returns an error immediately, so the whole
#                      espradio/lneto network stack becomes unreachable
#
# Measured on esp32s3-generic with "socket otlpjson":
#   endpoint only          flash 117,179   (wrong: no networking linked in)
#   endpoint + ssid        flash 733,999   (real)
#
# Never drop a variable from this list to "simplify" a measurement.
SIZE_LDFLAGS = -X main.ssid=size-measurement -X main.password=size-measurement \
	-X main.endpoint=http://192.0.2.1:4318/v1/metrics

# Builds every combination so the size table in the talk can be regenerated.
build-all: $(BUILD)
	@for t in esp32s3-generic xiao-esp32c3; do \
	  for tags in "" "socket" "otlpjson" "socket otlpjson"; do \
	    slug=$$(echo "$${tags:-nethttp-protobuf}" | tr ' ' '-'); \
	    out=$(BUILD)/device-$$t-$$slug.bin; \
	    $(TINYGO) build -target=$$t -tags "$$tags" -stack-size=$(STACK_SIZE) \
	      -ldflags "$(SIZE_LDFLAGS)" -o $$out ./cmd/device || exit 1; \
	    printf '%-28s %-26s %8d bytes\n' "$$t" "$$slug" $$(stat -c%s $$out); \
	  done; \
	done

flash:
	@test -n "$(SSID)" || { echo "SSID is required"; exit 1; }
	@test -n "$(ENDPOINT)" || { echo "ENDPOINT is required"; exit 1; }
	$(TINYGO) flash -target=$(TARGET) -tags "$(TAGS)" -stack-size=$(STACK_SIZE) -monitor \
		-ldflags "$(LDFLAGS)" ./cmd/device

monitor:
	$(TINYGO) monitor -target=$(TARGET)

# Prints the flash and RAM cost per package, which is where the net/http
# figures in the talk come from.
sizes:
	$(TINYGO) build -target=$(TARGET) -tags "$(TAGS)" -stack-size=$(STACK_SIZE) -ldflags "$(SIZE_LDFLAGS)" -size=full -o /dev/null ./cmd/device

collector:
	docker rm -f tinygo-otel-collector >/dev/null 2>&1 || true
	docker run -d --name tinygo-otel-collector \
		-p $(COLLECTOR_PORT):4318 \
		-v $(CURDIR)/collector/config.yaml:/etc/otelcol/config.yaml:ro \
		$(COLLECTOR_IMAGE) --config /etc/otelcol/config.yaml
	@echo "collector listening on :$(COLLECTOR_PORT); follow it with:"
	@echo "  docker logs -f tinygo-otel-collector"

collector-stop:
	docker rm -f tinygo-otel-collector >/dev/null 2>&1 || true

# Edge collector plus a Grafana stack (grafana/otel-lgtm) with the device
# dashboard provisioned. Grafana is on http://localhost:3000.
stack:
	docker rm -f tinygo-otel-collector >/dev/null 2>&1 || true
	COLLECTOR_PORT=$(COLLECTOR_PORT) docker compose -p tinygo up -d
	@echo "grafana:   http://localhost:3000/d/tinygo-device"
	@echo "collector: :$(COLLECTOR_PORT)  (docker compose -p tinygo logs -f collector)"

stack-stop:
	docker compose -p tinygo down

# Runs the same pipeline on this machine. If this succeeds and the device does
# not, the fault is on the device rather than in the collector or the payload.
hostsim:
	$(GO) run -tags "$(TAGS)" ./cmd/hostsim \
		-endpoint http://localhost:$(COLLECTOR_PORT)/v1/metrics -n 3 -interval 1s

encode:
	$(GO) run -tags "$(TAGS)" ./cmd/hostsim -encode-only -n 1

clean:
	rm -rf $(BUILD)
