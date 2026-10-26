# Changelog

Versionsnummer: `VERSION` (wird ins Programm eingebaut und oben in der Web-UI angezeigt).
Zu jeder Version gibt es ein Image `ghcr.io/wakkomon/llm-balancer:<version>`; `latest` ist immer der neueste Stand von `main`.

## 1.0.2 – 2026-10-26

- Fehler, die bisher beim Client (z. B. Hermes-Agent) ankamen und dort dessen eigenen Fallback auslösten, leitet der
  Balancer jetzt intern auf andere Server bzw. die Fallback-Gruppe um:
  - **alle** Fehlercodes werden wiederholt, auch 4xx wie 400 „request exceeds the available context size“
    (`RETRY_STATUS=all` ist Standard; mit einer Liste wie `RETRY_STATUS=404,429` gilt das alte Verhalten)
  - HTTP 200, aber das erste Stream-Event ist ein Fehler (llama.cpp), eine Fehlerzeile (Ollama) oder die Antwort ist leer:
    der Anfang jeder Antwort wird geprüft, bevor etwas an den Client geht
- Fehler, die erst nach Beginn der Antwort kommen (nicht mehr umleitbar) und an den Client durchgereichte Fehler stehen
  in „Letzte Anfragen“

## 1.0.1 – 2026-10-26

- Versionsnummer statt Branch/Commit in der Web-UI (Commit im Tooltip)
- Fallback greift jetzt auch für Anfragen, die schon in der Warteschlange warten, wenn alle Server ihrer Gruppe
  deaktiviert, entfernt oder down werden – vorher warteten sie bis zur Fallback-Wartezeit bzw. zum Queue-Timeout

## 1.0.0 – Stand vor der Versionierung

Slot-basiertes Balancing mit Queue, Web-UI (Übersicht, Gruppen, Statistik, Letzte Anfragen, API-Zugriff), Servergruppen
mit Modell-Routing, erzwungenem Modellnamen und Fallback-Gruppe, Wiederholung auf anderen Servern bei Fehlern,
Token-Zählung, Preise (eingehend/ausgehend), Tag-Prüfung, Tokens/s, Stop-Knopf, Bearer-Tokens, DNS_SERVERS, https.
