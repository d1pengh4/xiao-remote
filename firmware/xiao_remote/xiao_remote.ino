// XIAO ESP32S3 Sense: connect D1 (GPIO2) to GND to request playback.
// Arduino ESP32 core 3.3.12+; HTTPS uses the core's built-in CA bundle.
#include <Arduino.h>
#include <WiFi.h>
#include <NetworkClientSecure.h>
#include <HTTPClient.h>
#include <esp_system.h>
#include <mbedtls/md.h>
#include <atomic>
#include <time.h>
#include "config.h"

constexpr uint8_t TRIGGER_PIN = D1;
constexpr uint32_t DEBOUNCE_MS = 50;
constexpr uint32_t EVENT_TTL_MS = 15000;
struct PlayEvent { char id[32]; uint32_t created; };
QueueHandle_t events;
std::atomic<bool> pending(false);
bool rawPressed, stablePressed, armed = false, wasConnected = false;
uint32_t changedAt, bootA, bootB, eventCounter = 0, reconnectAt = 0;

bool clockReady() { return time(nullptr) >= 1704067200; }

void queuePlay() {
  bool expected = false;
  if (!pending.compare_exchange_strong(expected, true)) {
    Serial.println("Previous request pending; press ignored");
    return;
  }
  PlayEvent event;
  snprintf(event.id, sizeof(event.id), "%08lx%08lx-%08lx",
           (unsigned long)bootA, (unsigned long)bootB, (unsigned long)++eventCounter);
  event.created = millis();
  if (xQueueSend(events, &event, 0) != pdTRUE) pending.store(false);
  else Serial.println("Play request queued");
}

String signedMessage(const PlayEvent &event) {
  String stamp = String((unsigned long)time(nullptr));
  String canonical = String("v1\n") + event.id + "\n" + stamp;
  unsigned char digest[32];
  const auto *info = mbedtls_md_info_from_type(MBEDTLS_MD_SHA256);
  if (mbedtls_md_hmac(info, (const unsigned char *)SIGNING_KEY, strlen(SIGNING_KEY),
                      (const unsigned char *)canonical.c_str(), canonical.length(), digest) != 0)
    return "";
  char hex[65];
  for (size_t i = 0; i < 32; ++i) snprintf(hex + i * 2, 3, "%02x", digest[i]);
  return String("{\"v\":1,\"id\":\"") + event.id + "\",\"ts\":" + stamp +
         ",\"sig\":\"" + hex + "\"}";
}

void senderTask(void *) {
  PlayEvent event;
  for (;;) {
    if (xQueueReceive(events, &event, portMAX_DELAY) != pdTRUE) continue;
    while ((WiFi.status() != WL_CONNECTED || !clockReady()) &&
           millis() - event.created < EVENT_TTL_MS) vTaskDelay(pdMS_TO_TICKS(100));
    if (WiFi.status() == WL_CONNECTED && clockReady() &&
        millis() - event.created < EVENT_TTL_MS) {
      const String body = signedMessage(event);  // Reuse ID, timestamp and signature on retries.
      for (uint8_t attempt = 1; body.length() && attempt <= 3; ++attempt) {
        if (millis() - event.created >= EVENT_TTL_MS) break;
        NetworkClientSecure client;
        client.useBuiltinCACertBundle();  // Validate the certificate AND hostname; never setInsecure.
        client.setHandshakeTimeout(6);
        HTTPClient http;
        const String url = String("https://ntfy.sh/") + RELAY_TOPIC;
        int code = -1;
        if (http.begin(client, url)) {
          http.setConnectTimeout(3000);
          http.setTimeout(4000);
          http.addHeader("Content-Type", "text/plain; charset=utf-8");
          code = http.POST(body);
          http.end();
        }
        Serial.printf("Relay attempt %u, HTTP %d\n", attempt, code);
        if (code == 200) {
          Serial.println("Relay accepted (check Windows playback separately)");
          break;
        }
        if (code >= 400 && code < 500 && code != 429) break;
        if (attempt < 3) vTaskDelay(pdMS_TO_TICKS(code == 429 ? 5000 : 2000));
      }
    } else Serial.println("Request expired: check Wi-Fi and internet time sync");
    pending.store(false);
  }
}

void setup() {
  Serial.begin(115200);  // Standalone power works without a USB serial monitor.
  pinMode(TRIGGER_PIN, INPUT_PULLUP);
  rawPressed = stablePressed = digitalRead(TRIGGER_PIN) == LOW;
  changedAt = millis();
  bootA = esp_random(); bootB = esp_random();
  events = xQueueCreate(1, sizeof(PlayEvent));
  if (!events || xTaskCreate(senderTask, "https-sender", 12288, nullptr, 1, nullptr) != pdPASS) {
    Serial.println("Sender initialization failed; restarting");
    delay(1000); ESP.restart();
  }
  WiFi.mode(WIFI_STA);
  WiFi.setAutoReconnect(true);
  WiFi.begin(WIFI_SSID, WIFI_PASSWORD);
  configTime(0, 0, "time.cloudflare.com", "pool.ntp.org", "time.google.com");
  Serial.println("Ready: release D1-GND for 50ms, then connect to trigger");
}

void loop() {
  const uint32_t now = millis();
  const bool connected = WiFi.status() == WL_CONNECTED;
  if (connected != wasConnected) {
    wasConnected = connected;
    Serial.println(connected ? "WIFI_CONNECTED" : "WIFI_DISCONNECTED");
  }
  while (Serial.available()) {
    const char command = Serial.read();
    if (command == 't') queuePlay();
    if (command == 's') Serial.printf("STATUS wifi=%d clock=%d D1=%s pending=%d\n",
        connected, clockReady(), digitalRead(TRIGGER_PIN) == LOW ? "LOW" : "HIGH", pending.load());
  }
  const bool pressed = digitalRead(TRIGGER_PIN) == LOW;
  if (pressed != rawPressed) { rawPressed = pressed; changedAt = now; }
  if (now - changedAt >= DEBOUNCE_MS) {
    if (!rawPressed) armed = true;
    if (stablePressed != rawPressed) {
      stablePressed = rawPressed;
      if (stablePressed && armed) { armed = false; Serial.println("Contact detected"); queuePlay(); }
    }
  }
  if (!connected && now - reconnectAt >= 10000) { reconnectAt = now; WiFi.reconnect(); }
  delay(5);  // HTTPS runs in a separate task so contact sampling continues.
}
