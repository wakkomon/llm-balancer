# Changelog

Versionsnummer: `VERSION` (wird ins Programm eingebaut und oben in der Web-UI angezeigt).
Zu jeder Version gibt es ein Image `ghcr.io/wakkomon/llm-balancer:<version>`; `latest` ist immer der neueste Stand von `main`.

## 1.14.0 – 2026-11-08

- **HTTPS-Port** zusätzlich zum http-Port (`HTTPS_LISTEN`, Standard `:8443`, `off` = aus) mit derselben API, Web-UI und
  MCP-Schnittstelle; der http-Port läuft unverändert weiter
- **Selbst signiertes Zertifikat** wird automatisch in `/data/tls/` erzeugt (ECDSA P-256, 825 Tage) und vor Ablauf
  erneuert; Namen/IPs: localhost, Container, `TLS_HOSTS` und in der UI eingetragene. Alternativ eigenes Zertifikat über
  `TLS_CERT`/`TLS_KEY` (wird bei Änderung ohne Neustart neu geladen)
- Einstellungen → **HTTPS**: Gültigkeit, Namen, SHA-256-Fingerabdruck, Zertifikat herunterladen, mit weiteren Namen neu
  erzeugen
- Docker-Image und Stack-Vorlagen geben Port 8443 frei

## 1.13.3 – 2026-11-05

- Sessions: das Feld `user` bestimmt die Session nicht mehr – es nennt den Benutzer, nicht das Gespräch, und hat alle
  Gespräche eines Benutzers zu einer Session zusammengefasst (steht weiter in den Session-Details). `metadata.user_id`
  zählt nur noch, wenn es eine Session enthält (Claude Code). Neu erkannt: Header `X-OpenWebUI-Chat-Id`
- Verknüpfung nach Kontext-Kompression: Ein neues Gespräch, das nur mit denselben eingefügten Nachrichten beginnt
  (Memory, Skills, Anweisungen), wird nicht mehr an eine laufende Session gehängt – verknüpft wird nur, wenn die neue
  Anfrage die letzten Nachrichten der alten Session enthält

## 1.13.2 – 2026-11-03

- „freie Slots“: Server, die nicht erreichbar sind oder am Kostendeckel stehen, zählen in der Gesamtzahl auch nicht mehr
  mit (wie schon deaktivierte) – außer sie haben noch laufende Anfragen

## 1.13.1 – 2026-11-03

- **Deutlich weniger CPU pro Anfrage** (bei großen Agenten-Anfragen etwa 7-mal weniger): Der Anfrage-Body wird nur noch
  einmal gelesen statt bis zu zehnmal (Token-Prüfung, Session, Kompressions-Verknüpfung, Session-Details, Router,
  Klassifizierer); Modellname und `stream_options` werden direkt im Body ersetzt statt ihn neu zu kodieren; reguläre
  Ausdrücke über den ganzen Verlauf (`<think>`, `<system-reminder>`, Leerraum) ersetzt durch Prüfungen, die nur so viel
  lesen wie nötig
- Der Router analysiert die Anfrage nicht mehr, während er die interne Sperre hält – andere Anfragen und die Statusseite
  warten nicht mehr darauf
- CPU-Profile für Administratoren unter `/balancer/debug/pprof/` (für die Fehlersuche bei hoher Last)

## 1.13.0 – 2026-11-03

- **CPU-Last des Balancer-Rechners** oben im Kopf: Anteil des Balancer-Prozesses und Last des ganzen Hosts (gelb ab
  60 %, rot ab 85 %), im Tooltip Load Average, Spitzen der letzten 5 Minuten, RAM von Balancer und Host,
  Container-Limits. Auch in `/balancer/metrics` (`llm_balancer_process_cpu_percent`, `llm_balancer_host_cpu_percent`,
  `llm_balancer_host_load1`, `llm_balancer_process_rss_bytes`, `llm_balancer_goroutines`), in `/api/status` (`system`)
  und im MCP-Tool `get_status`
- „freie Slots“: die Gesamtzahl zählt nur noch eingeschaltete Server (deaktivierte Server und Server in deaktivierten
  Gruppen nicht mehr)

## 1.12.1 – 2026-11-03

- Bricht ein Client eine Anfrage ab (Timeout, Stop in Open WebUI, Agent-Abbruch) oder wird sie per Stop beendet, gab der
  Balancer den Slot sofort frei – der LLM-Server rechnete die abgebrochene Anfrage aber oft noch weiter und bekam schon
  die nächste. Dann arbeitete ein Server mit einem Slot an zwei Anfragen, während die UI nur eine zeigte. Jetzt bleibt
  der Slot nach einem Abbruch `ABORT_GRACE_SEC` (Standard 3 s) belegt; die Karte zeigt ihn schraffiert („Anfrage
  abgebrochen – Slot wird gleich frei“)
- Übersicht: neue Kachel **laufend** (Anfragen); die Zustands-Kacheln sagen jetzt dazu, dass sie **Server** zählen
- Ein Server, der deaktiviert, down oder am Deckel ist, zeigt „· N läuft noch“, solange Anfragen auf ihm zu Ende laufen
- Wird ein Server per Konfigurations-Import mit neuer URL ersetzt, behält er seine laufenden Anfragen im Zähler
- Interne Datenrennen beim Anlegen einer Anfrage behoben (Status las Felder, die noch geschrieben wurden)

## 1.12.0 – 2026-11-02

- **MCP-Schnittstelle** unter `/mcp` (Model Context Protocol, Streamable HTTP): KI-Agenten können Status, Anfragen,
  Sessions, Statistik, Tagesansicht, Modelle und Ereignisse abfragen, Router testen, Server und Gruppen schalten,
  Anfragen abbrechen und Ollama-Modelle entladen, laden und löschen
- Geschützt mit eigenen **MCP-Tokens mit Rolle** (Betrachter / Operator / Administrator) – die Rolle bestimmt, welche
  Tools der Agent sieht. Verwaltung unter API-Zugriff → MCP-Zugriff mit fertiger Client-Konfiguration zum Kopieren;
  gespeichert nur als Hash in `/data/mcp.json`, im Konfigurations-Export enthalten

## 1.11.4 – 2026-11-02

- Anleitung und Screenshots auf den aktuellen Stand gebracht (Anfragen/s, Statistik nach Gruppe, Sessions mit
  laufenden Anfragen, Session-Details, Token-Dialog); Einzahl/Mehrzahl in den Session-Details

## 1.11.3 – 2026-11-02

- Session-Details: „letzte Nachricht“ fehlte bei Agenten-Läufen (die Nutzer-Nachricht bleibt dort gleich und wurde
  deshalb ausgeblendet). Jetzt getrennt: letzte Nutzer-Nachricht, letzte Nachricht im Verlauf (mit Rolle – auch
  Tool-Ergebnisse und Tool-Aufrufe) und der Anfang der letzten erzeugten Antwort

## 1.11.2 – 2026-11-02

- „Letzte Anfragen“ zeigt auch laufende (▶ Prompt … / streamt) und wartende Anfragen – bisher erschienen sie erst nach
  dem Ende; in der Session-Ansicht zählt die Session-Zeile sie mit (▶ laufend, ⏳ wartend)

## 1.11.1 – 2026-11-02

- **Sessions überstehen Kontext-Kompression**: ersetzt ein Agent ältere Nachrichten durch eine Zusammenfassung, wird der
  neue Fingerabdruck über gemeinsame jüngste Nachrichten bzw. die letzte Antwort der bisherigen Session zugeordnet
  (gleicher API-Token und System-Prompt, letzte 3 h; von mehreren Sessions geteilte Texte zählen nicht).
  Die Session-Details zeigen „Kontext komprimiert: n×“

## 1.11.0 – 2026-11-02

- **Session-Details** in „Letzte Anfragen“: Klick auf die Session-Kennung öffnet ein Fenster mit Erkennung (inkl. vom
  Client gesendeter ID), Zeitraum, API-Token, Quell-IP, X-Forwarded-For, User-Agent, Modellen, identifizierenden Headern
  und Body-Feldern, Anfang des System-Prompts, erster und letzter Nachricht; Filter auf die Session von dort aus.
  API: `GET /balancer/api/sessions/{id}` (nur im Speicher, letzte 500 Sessions)
- In der Session-Ansicht steht unter der Kennung der Titel bzw. Anfang der ersten Nachricht

## 1.10.0 – 2026-11-01

- Übersicht: Kachel **Anfragen/s** (Ø der letzten 10 s) statt „Anfragen gesamt“; „abgelehnt“ und „Deckel“ entfallen
  (Gesamtzahlen im Tooltip, Kostendeckel weiter auf der Server-Karte). Status-API: `live.rps`
- Statistik: Aufteilung **nach Gruppe** (Server nach ihrer aktuellen Gruppe zusammengezählt, ohne abgelehnte Anfragen)
- Behoben: Umschalter (Zeitraum, Aufteilung, Ansicht …) aktualisierten Tabellen teils erst beim nächsten Refresh

## 1.9.1 – 2026-11-01

- **Letzte Anfragen nach Session gruppiert** (Standard): eine Zeile pro Session mit Anzahl, Modellen, Tokens, Servern,
  Erfolg/Fehler und Summen (Wartezeit, Rechenzeit, Tokens, Einnahmen, Kosten); die einzelnen Anfragen aufklappbar.
  Umschalter „nach Session / einzeln“, Sortierung hält die aufgeklappten Anfragen unter ihrer Session

## 1.9.0 – 2026-11-01

- **Live-Durchsatz**: erzeugte Tokens pro Sekunde (Ø der letzten 10 s) – als Kachel in der Übersicht, pro API-Token
  und als Summe unter API-Zugriff, dort auch die aktiven (laufenden + wartenden) Anfragen je Token.
  Status-API: `live` mit `running`, `queued`, `tps` sowie `tokens` und `backends`
- Kacheln der Übersicht etwas kompakter beschriftet („wartend“, „Deckel“), damit alle in eine Zeile passen

## 1.8.2 – 2026-10-31

- Anleitung: Hinweis, dass das Projekt KI-generiert ist; neuer Abschnitt „Funktionsumfang“ und „Bedienung“ mit
  Screenshots aller Seiten (`docs/screenshots/`)
- Behoben: nach dem Umschalten auf die Listenansicht (oder Auf-/Zuklappen einer Gruppe) blieb die Liste bis zur
  nächsten Aktualisierung leer

## 1.8.1 – 2026-10-30

- Umbenennen eines Servers oder API-Tokens überträgt seine Statistik (Tage, Stunden, Einnahmen) auf den neuen Namen
- Reste früherer Umbenennungen lassen sich in der Statistik bzw. unter API-Zugriff per „zusammenführen …“ auf einen
  vorhandenen Server/Token übertragen (`POST /balancer/api/stats/merge {"kind":"backend|token","from","to"}`)

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
