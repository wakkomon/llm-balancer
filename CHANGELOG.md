# Changelog

Versionsnummer: `VERSION` (wird ins Programm eingebaut und oben in der Web-UI angezeigt).
Zu jeder Version gibt es ein Image `ghcr.io/wakkomon/llm-balancer:<version>`; `latest` ist immer der neueste Stand von `main`.

## 1.8.0 – 2026-10-30

- **API-Tokens bearbeiten**: Name, erlaubte Modelle (mit `*`), Preise, Kostendeckel, neuer Token-Wert. Nicht erlaubte
  Modelle → HTTP 403 `model_not_allowed`; `/v1/models` und `/api/tags` zeigen pro Token nur die erlaubten
- Router: Vorschläge funktionieren jetzt auch nach dem Komma (mehrere Modelle in Regel-Ziel, Standardmodell, Stufen, Routen)

## 1.7.1 – 2026-10-29

- **Router: mehrere gleichwertige Zielmodelle** (mit Komma) für Regeln, Standardmodell, Stufen und Routen – die Anfrage
  nimmt den ersten freien Server mit einem davon bzw. wartet auf den zuerst frei werdenden; der Modellname wird passend
  zum Server gesetzt. „Fähigkeiten beachten“ sortiert ungeeignete Modelle aus der Liste aus

## 1.7.0 – 2026-10-29

- **Klassifizierer für Router** (wenn keine Regel passt), angelehnt an LiteLLMs Auto Routing:
  - **Heuristik**: Komplexitäts-Score ohne Modellaufruf → Stufe SIMPLE / MEDIUM / COMPLEX / REASONING
  - **LLM**: ein kleines Modell ordnet einer Stufe zu
  - **Arch-Router**: `arch-router:1.5b` (katanemo/Arch-Router-1.5B) wählt eine beschriebene Route
- Stufen mit Modell-Pools (erstes mit freiem Server, leere Stufe → nächsthöhere); Regeln können eine Stufe als Ziel haben
- Klassifizierer laufen am Slot-System vorbei, mit Timeout und Rückfall auf Standardmodell oder Heuristik;
  Anzeige von Stufe/Route und Dauer in „Letzte Anfragen“ und im Test

## 1.6.0 – 2026-10-29

- **Modell-Router** (neue Seite „Router“): virtuelle Modelle wie `auto`, die per Regeln das echte Modell wählen – nach
  geschätzter Prompt-Größe, Bildern, Tools, JSON-Antwort, angefordertem Denken, Text-Muster, API-Token, Client,
  Header, Warteschlange, belegtem Modell und Uhrzeit; Ziel ist ein Modell oder „ein Modell mit Fähigkeit X“.
  Routers können auch echte Modellnamen übernehmen (Muster), haben ein Standardmodell, optional Session-Bindung und
  „Fähigkeiten beachten“. Treffer pro Regel, Test-Werkzeug, Anzeige in „Letzte Anfragen“; Router erscheinen in
  `/v1/models` und sind Teil des Konfigurations-Exports
- **Modell-Fähigkeiten pro Server** (Bilder, Tools, Denken, Kontext), bei Ollama automatisch aus `/api/show`;
  Anzeige auf der Seite Modelle

## 1.5.0 – 2026-10-29

- **Ollama-Unterstützung**: Server-Typ automatisch / OpenAI-kompatibel / Ollama (Erkennung über `/api/version`),
  Badge mit Version und geladene Modelle auf der Karte
- **Native Ollama-API am Balancer**: `/api/chat`, `/api/generate`, `/api/embed(dings)` mit Slots, Gruppen und Fallback
  nur zu Ollama-Servern; `/api/tags`, `/api/ps`, `/api/version` über alle Ollama-Server zusammengefasst
- **Modellverwaltung** (neue Seite „Modelle“): Modelle aller Ollama-Server mit Größe, Parametern, Quantisierung und
  Speicherbelegung; herunterladen auf einen oder mehrere Server mit Fortschritt und Abbrechen, auf weitere Server
  kopieren, löschen, aus dem Speicher entladen

## 1.4.1 – 2026-10-27

- **Modell erzwingen pro Server**: Ersatzmodell, das gesendet wird, wenn der Server das angefragte Modell nicht exakt
  kennt (Treffer über `*` im Filter oder als Teil seiner Gruppe); exakt bekannte Modelle bleiben unverändert

## 1.4.0 – 2026-10-27

- **Routing innerhalb einer Gruppe nach Modell**: haben die Server einer Gruppe verschiedene Modelle, bekommt eine Anfrage
  nur die Server, die das Modell haben (bisher jeder Server der Gruppe – mit Fehlversuch und Weiterleitung); sind sie
  belegt, wartet sie auf diese. Kennt kein Server der Gruppe das Modell, darf es wie bisher die ganze Gruppe. Gilt auch
  für die Fallback-Gruppe (mit dem dort eingestellten Modell)
- **Modellfilter pro Server**: Namen/Muster im Dialog „Bearbeiten“, optional ohne die vom Server gemeldeten Modelle
- **Exakt vor Muster**: ein exakt bekanntes Modell (gemeldet oder ohne `*` eingetragen) hat Vorrang vor `*`-Treffern –
  bei der Wahl der Gruppe und des Servers

## 1.3.0 – 2026-10-27

- **Benutzerverwaltung** (Einstellungen): Anmeldung mit Benutzername + Passwort, Rollen Administrator / Operator
  (Server und Gruppen an/aus, Stop) / Betrachter (nur lesen). Sobald es einen Benutzer gibt, braucht die ganze Web-UI und
  `/balancer/api/` eine Anmeldung; die LLM-API, `/healthz` und `/metrics` nicht. Erster Admin über die UI oder
  `ADMIN_USER` / `ADMIN_PASSWORD`. Passwort-Hashes (PBKDF2-SHA256), Sitzungen 30 Tage, Sperre nach 5 Fehlversuchen,
  Schutz des letzten Admins; `ADMIN_TOKEN` bleibt als Admin-Zugang für Skripte
- **Konfiguration exportieren/importieren** (Einstellungen): Server und Gruppen, API-Tokens, E-Mail-Einstellungen,
  Benutzer – wählbar, Passwörter/Tokens optional; Import mit Vorschau, fehlende Geheimnisse bleiben erhalten
- Ohne Rechte zum Ändern werden die entsprechenden Knöpfe ausgeblendet

## 1.2.0 – 2026-10-27

- **Tagesansicht** unter Statistik → „Tag“: Auslastung pro Stunde (Anteil der Slot-Zeit, in der gerechnet wurde) heute
  gegen den Ø der letzten 7 Tage, umschaltbar auf Anfragen und wartende Anfragen pro Stunde, für alle Server oder einen;
  Kacheln für Auslastung jetzt, heute bis jetzt, Ø ganzer Tag, Anfragen und Wartende; Tabelle pro Server.
  Stundenwerte werden ab dieser Version erfasst (62 Tage). API: `GET /balancer/api/stats/day`
- **Statistik der Warteschlange** pro Tag: Anfragen, gewartet (≥ 0,25 s), Ø/max. Wartezeit, max. Länge, abgelehnt
- **Sessions** in „Letzte Anfragen“: Kennung aus Session-Header/-Feld des Clients oder Fingerabdruck aus System-Prompt +
  erster Nachricht; trennt verschiedene Unterhaltungen desselben API-Tokens; Klick filtert auf die Session
- **Gruppen deaktivierbar** (Seite Gruppen): ihre Server bekommen keine Anfragen, die Fallback-Gruppe übernimmt sofort –
  auch schon wartende Anfragen. API: `POST /balancer/api/groups/{name}/disable|enable`
- Alle Tabellen sortierbar (Klick auf die Spaltenüberschrift; Reihenfolge bleibt beim Aktualisieren erhalten)
- Report: Ereignisse nach Text und Ereignisart filterbar
- API-Zugriff: Anfragen, Tokens und Einnahmen pro Token wahlweise für heute / 7 / 30 / 90 Tage / 1 Jahr / gesamt, mit
  Summenzeile (ersetzt die Tabelle „Einnahmen pro API-Token“ in der Statistik)
- Übersicht: Kacheln „beschäftigt“ und „down“ nicht mehr hervorgehoben; beschäftigte Server zeigen statt des Punkts einen
  pulsierenden blauen Play-Button (Karte, Liste, Gruppen)

## 1.1.2 – 2026-10-26

- Übersicht umgebaut: die Zähler beschäftigt / idle / fehlerhaft / down / Kostendeckel / deaktiviert stehen als Kacheln in
  der ersten Zeile neben „freie Slots“ (leuchten auf, wenn > 0; Tooltip nennt die Server); die Zeile mit den
  Server-Chips entfällt
- „beschäftigt“ ist animiert: pulsierender Punkt im Status der Server-Karte, laufende Streifen im Slot-Balken,
  pulsierender Punkt in Kachel und Listenansicht (aus bei „Bewegung reduzieren“ im Betriebssystem)
- Behoben: JavaScript-Fehler bei einer neuen Installation ohne Statistikdaten
- Tab-Leiste auf dem Handy scrollbar statt umgebrochen

## 1.1.1 – 2026-10-26

- „Tokens Ein/Aus“ und „Preis Ein/Aus“ jetzt aus der richtigen Sicht: bei API-Tokens aus Sicht der Anwendung
  (Aus = Prompt, Ein = Antwort – vorher vertauscht), bei Servern und Modellen aus Sicht des Servers (Ein = Prompt,
  Aus = Antwort); jede Spalte nennt zusätzlich „Prompt“ bzw. „Antwort“. CSV: `Tokens_Prompt` / `Tokens_Antwort`

## 1.1.0 – 2026-10-26

- **Kostendeckel für API-Tokens** (eingehend, pro Tag / Monat / gesamt): bei Erreichen werden Anfragen mit dem Token mit
  HTTP 429 `insufficient_quota` abgelehnt, im neuen Zeitraum oder nach Erhöhen des Deckels automatisch wieder frei;
  Tokens lassen sich außerdem manuell sperren (HTTP 403)
- **Kostendeckel für Server** (ausgehend): bei Erreichen bekommt der Server keine Anfragen mehr (Zustand „Kostendeckel“),
  die Fallback-Logik greift sofort – auch für schon wartende Anfragen
- **Report-Seite** (`#report`): Ereignisprotokoll und E-Mail-Benachrichtigung per SMTP (STARTTLS, SSL/TLS oder
  unverschlüsselt) bei Server nicht erreichbar / liefert Fehler / wieder da, Kostendeckel erreicht / wieder frei;
  Verzögerung gegen kurze Aussetzer, Testmail
- Deckel werden schon beim Start geprüft (gilt nach einem Neustart sofort)

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
