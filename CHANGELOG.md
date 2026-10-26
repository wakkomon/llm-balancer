# Changelog

Versionsnummer: `VERSION` (wird ins Programm eingebaut und oben in der Web-UI angezeigt).
Zu jeder Version gibt es ein Image `ghcr.io/wakkomon/llm-balancer:<version>`; `latest` ist immer der neueste Stand von `main`.

## 1.0.1 – 2026-10-26

- Versionsnummer statt Branch/Commit in der Web-UI (Commit im Tooltip)
- Fallback greift jetzt auch für Anfragen, die schon in der Warteschlange warten, wenn alle Server ihrer Gruppe
  deaktiviert, entfernt oder down werden – vorher warteten sie bis zur Fallback-Wartezeit bzw. zum Queue-Timeout

## 1.0.0 – Stand vor der Versionierung

Slot-basiertes Balancing mit Queue, Web-UI (Übersicht, Gruppen, Statistik, Letzte Anfragen, API-Zugriff), Servergruppen
mit Modell-Routing, erzwungenem Modellnamen und Fallback-Gruppe, Wiederholung auf anderen Servern bei Fehlern,
Token-Zählung, Preise (eingehend/ausgehend), Tag-Prüfung, Tokens/s, Stop-Knopf, Bearer-Tokens, DNS_SERVERS, https.
