# llm-balancer

Kleiner, abhängigkeitsfreier Reverse-Proxy für lokale LLM-Server (llama.cpp, Ollama, vLLM, LM Studio …),
die nur **eine Anfrage gleichzeitig** sinnvoll verarbeiten können.

Statt zufällig zu verteilen (wie LiteLLM) zählt der Balancer die **offenen Verbindungen pro Server**:

* Ist ein Server frei → Anfrage geht sofort dorthin.
* Ist er belegt → nächster freier Server in der Liste.
* Sind alle belegt → Anfrage wartet in einer **FIFO-Warteschlange**, bis irgendein Slot frei wird.

Request und Response werden **1:1 durchgereicht** (Body, Header, SSE-Streaming ohne Pufferung).
Modellname & Co. werden nicht ausgewertet – nur zur Anzeige in der Web-UI gelesen.

```
Hermes-Agent ─► LiteLLM ─► llm-balancer :8080 ─┬─► LLM-Server A (1 Slot)
                                               ├─► LLM-Server B (1 Slot)
                                               └─► LLM-Server C (1 Slot)
```

## Funktionen

* Slot-Zählung pro Backend (`max_concurrent`, Standard 1). Der Slot wird erst freigegeben, wenn der Stream komplett beendet ist oder der Client abbricht
* FIFO-Queue mit optionalem Limit (`max_queue`) und Timeout (`queue_timeout_sec`)
* Priorität: Bei mehreren freien Servern wird der mit kleinerer `priority` bevorzugt (z. B. die schnellere GPU), sonst Round-Robin
* Failover: Verbindungsfehler oder HTTP 503 (z. B. llama.cpp „no slot available“ / „loading model“) → automatisch nächster Server; wiederholte Anfragen rücken an die Spitze der Queue
* Health-Checks (Standard `GET /v1/models` alle 15 s) für **freie** Server; ausgefallene Server werden übersprungen, bis sie wieder antworten
* `GET`-Anfragen (z. B. `/v1/models`, Health-Checks von LiteLLM) belegen keinen Slot und warten nie
* **Servergruppen mit Routing nach Modell**: Anfragen gehen nur an die Server, die das angefragte Modell bedienen – pro Gruppe mit eigener, fairer Warteschlange (ist die eine Gruppe ausgelastet, warten Anfragen für andere Gruppen nicht mit)
* Modellerkennung: die Modelle jedes Servers werden per `/v1/models` abgefragt; `GET /v1/models` am Balancer liefert die Modelle aller Server
* **Token-Zählung und Preise**: Prompt- und Antwort-Tokens jeder Anfrage aus der Antwort (OpenAI, Anthropic, Ollama, llama.cpp), Preis pro 1 Mio. Tokens je API-Token, Kosten pro Token und Tag
* **Fallback-Gruppe** pro Gruppe: wartet eine Anfrage länger als eingestellt (oder ist die ganze Gruppe down), dürfen Server einer anderen Gruppe sie übernehmen – optional mit ersetztem Modellnamen
* **Prüfung des generierten Texts**: Tags wie `<think>` oder `<tool_call>` müssen korrekt geöffnet und geschlossen sein, Fehler werden pro Anfrage, Server, Modell und Tag angezeigt
* **Tokens pro Sekunde** pro Anfrage und Server (Generierung und Prompt-Verarbeitung), live geschätzt während des Streams
* Statistik auch **nach Modell**
* Status-Übersicht pro Server: **beschäftigt**, **idle**, **fehlerhaft** (erreichbar, aber letzte Anfrage fehlgeschlagen – bis zur nächsten erfolgreichen, max. 15 min), **down** (Health-Check/Verbindung fehlgeschlagen), **deaktiviert**
* Statistik pro Tag und Server (7/30/90/365 Tage): Anfragen, Fehler, Abbrüche, Rechenzeit, Ø/Max-Dauer, Ø Zeit bis zum ersten Byte, Ø Wartezeit in der Queue, Datenmenge – als gestapelte Balkengrafik und Tabelle, CSV-Export; gespeichert in `/data/stats.json`
* Web-UI unter `/balancer/` mit Tabs Übersicht, Gruppen (`#gruppen`), Statistik (`#statistik`), Letzte Anfragen (`#anfragen`) und API-Zugriff (`#zugriff`); Server als Kacheln mit fester Größe oder als kompakte Liste (eine Zeile pro Server, nach Gruppen, auf-/zuklappbar): Slots, laufende Anfragen (Prompt-Verarbeitung vs. Streaming, Dauer, Bytes), Warteschlange, letzte Anfragen, Ø Zeit bis zum ersten Byte / Ø Dauer, Server per Klick deaktivieren (drain) und aktivieren
* Server-Liste **im laufenden Betrieb** in der Web-UI ändern: hinzufügen, bearbeiten (URL, Name, Slots, Priorität), entfernen – ohne Neustart, laufende Anfragen laufen weiter
* Bearer-Token pro LLM-Server (z. B. für vLLM `--api-key` oder einen vorgeschalteten Proxy), in der UI hinterlegbar
* Zugriffsschutz für die LLM-API des Balancers mit eigenen Tokens (aus `API_KEYS` oder in der UI erzeugt, nur als Hash gespeichert)
* JSON-API `/balancer/api/status`, Prometheus-Metriken `/balancer/metrics`
* https-Server mit öffentlichen Zertifikaten oder (per Option „TLS-Zertifikat nicht prüfen“) selbstsignierten
* Eine statische Go-Binary ohne Abhängigkeiten, Docker-Image `FROM scratch` (~7 MB)

## Start

### docker compose / Portainer-Stack

```yaml
services:
  llm-balancer:
    image: ghcr.io/wakkomon/llm-balancer:latest   # oder: build: .
    restart: unless-stopped
    ports: ["8080:8080"]
    environment:
      BACKENDS: "gpu=http://192.168.1.50:8080,amd=http://192.168.1.60:8080@1"
    volumes:
      - llm-balancer-data:/data
volumes:
  llm-balancer-data:
```

Vollständige Vorlage: `portainer-stack.yml`.

Das Image wird von der GitHub-Action bei jedem Push auf `main` nach `ghcr.io/wakkomon/llm-balancer:latest` gebaut,
zusätzlich als `ghcr.io/wakkomon/llm-balancer:<version>` laut Datei `VERSION` (z. B. `:1.0.1`, dazu Git-Tag `v1.0.1`) – damit lässt sich eine Version festnageln
(beim ersten Mal das Package unter GitHub → Packages auf „public“ stellen oder `docker login ghcr.io`).

### Ohne Docker

```bash
go build -o llm-balancer .
BACKENDS="http://host-a:8080,http://host-b:8080" ./llm-balancer
```

## Konfiguration

### Server-Liste: Startwert und Änderungen in der UI

`BACKENDS` (bzw. `backends` in `config.json`) ist nur die **Startkonfiguration**. Sobald du die Liste in der
Web-UI änderst, wird sie in `DATA_DIR/backends.json` gespeichert (im Container `/data/backends.json`) und hat ab
dann Vorrang – auch nach Neustarts und Image-Updates, solange `/data` auf einem Volume liegt.
Zurück zu `BACKENDS` geht es per `POST /balancer/api/reset` (löscht die Datei; Gruppen werden dabei aus `config.json` übernommen).

Änderungen wirken sofort:

* Neuer Server oder mehr Slots → wartende Anfragen aus der Queue starten sofort dort.
* Geänderte URL → sofortiger Health-Check; laufende Anfragen auf der alten URL laufen zu Ende.
* Entfernter Server → bekommt keine neuen Anfragen mehr, laufende werden normal beendet.

Mit `ADMIN_TOKEN` lassen sich Änderungen absichern; die UI fragt dann einmal nach dem Token und merkt ihn sich im Browser.
Ohne Token kann jeder, der die UI erreicht, die Server-Liste ändern.

Für einen Bind-Mount statt eines Docker-Volumes muss der Ordner dem User 65534 gehören
(`chown 65534:65534 ./data`), sonst kann der Container nicht speichern – die UI zeigt das dann als Warnung an.

### Servergruppen und Modelle

Eine Gruppe ist eine Menge von Servern plus die Modelle, die sie bedienen. Ein Server gehört zu höchstens einer Gruppe
(Dialog „Bearbeiten“ des Servers oder Dialog der Gruppe). Die Zuordnung einer Anfrage passiert über das Feld `model`:

1. Gruppen, deren Modellliste passt – eingetragene Namen oder Muster mit `*` (z. B. `qwen3*`), und, wenn
   „zusätzlich gemeldete Modelle“ aktiv ist (Standard), alle Modelle, die ihre Server über `/v1/models` melden.
   Passen mehrere Gruppen, zählen die Server aller passenden Gruppen.
2. Sonst: alle Server, die das Modell selbst melden (auch ohne Gruppe).
3. Sonst: alle Server – oder 404, wenn `UNKNOWN_MODEL=reject` gesetzt ist.

Groß-/Kleinschreibung spielt keine Rolle. Die Modelle der Server werden beim Health-Check (alle 15 s, nur bei freien Servern)
neu abgefragt. Im Feld **Modell** über der Serverliste lässt sich ein Modell auswählen: die UI zeigt dann, welche Gruppe
und welche Server es bekommen, und blendet die übrigen Server aus. `GET /balancer/api/route?model=…` liefert dasselbe als JSON.

Ohne Gruppen verhält sich der Balancer wie bisher (alle Server bedienen alle Modelle).

### Was wird wann gespeichert?

Alles liegt im Volume `/data` (`DATA_DIR`):

| Datei | Inhalt | Geschrieben |
|---|---|---|
| `backends.json` | Server, Gruppen, Server-Tokens | sofort bei jeder Änderung |
| `access.json` | API-Tokens (Hash), Preise, verbrauchte Tokens und Kosten | sofort bei Änderungen, Zähler jede Minute |
| `stats.json` | Tagesstatistik pro Server, Modell und API-Token | jede Minute |
| `counters.json` | Zähler der Server-Karten (Anfragen, Fehler, Ø Dauer, tok/s, Tag-Fehler, letzter Fehler) und der Kacheln oben | jede Minute |

Minütlich wird nur geschrieben, was sich geändert hat, jeweils über eine temporäre Datei und Umbenennen (kein halb
geschriebener Stand nach einem Absturz). Beim Stoppen (SIGTERM) schreibt der Balancer sofort alles, wartet dann bis zu 30 s
auf laufende Anfragen und schreibt danach noch einmal. Damit Docker so lange wartet, steht in der Stack-Vorlage
`stop_grace_period: 40s` (Docker-Standard sind 10 s; ohne die Zeile gehen nur die Anfragen verloren, die beim Stoppen noch liefen).
Nach einem Absturz oder Stromausfall fehlt höchstens die letzte Minute. Nur die Liste „Letzte Anfragen“ liegt ausschließlich
im Speicher. „Statistik löschen“ setzt auch die Zähler der Server-Karten und Kacheln zurück.

### Modellname erzwingen

Pro Gruppe lässt sich ein Modellname eintragen, der an die Server der Gruppe geschickt wird – egal welcher Name angefragt
wurde (z. B. weil Ollama auf einem Server `qwen3:32b-q4_K_M` heißt, die Clients aber `qwen3-32b` anfragen). Die Zuordnung
zur Gruppe läuft weiter über den angefragten Namen; mit dem Muster `*` nimmt die Gruppe jedes Modell an. Übernimmt ein
Server der Gruppe eine Anfrage als Fallback, gilt das „Modell dort“ der Fallback-Einstellung, falls gesetzt, sonst der
erzwungene Name dieser Gruppe. In „Letzte Anfragen“ steht der angefragte und der tatsächlich gesendete Name.

### Was passiert, wenn ein Server einen Fehler liefert?

1. Antwortet ein Server mit einem Fehler, versucht der Balancer **einen anderen Server derselben Gruppe**
   (bzw. – ohne Gruppen – einen anderen passenden Server). Ist der gerade belegt, wartet die Anfrage an der Spitze der Queue.
2. Sind **alle Server der Gruppe** fehlgeschlagen (oder down), geht die Anfrage an die **Fallback-Gruppe** – sofort,
   ohne die eingestellte Wartezeit abzuwarten.
3. Ist auch die Fallback-Gruppe erschöpft (oder keine eingestellt), darf **jeder andere Server, der das Modell selbst meldet**
   (`/v1/models`), die Anfrage übernehmen – auch aus einer anderen Gruppe.
4. Erst wenn auch dort alle Server scheitern, bekommt der Client **die letzte echte Fehlerantwort** (Statuscode und Text
   des Servers, Header `X-LLM-Balancer-Attempts`). Waren alle Server unerreichbar, kommt HTTP 502 mit allen Verbindungsfehlern.

Anfragen ohne Slot (z. B. `GET`) gehen an einen Server und bei einer Fehlerantwort an den nächsten.
`GET /v1/models/<modell>` beantwortet der Balancer selbst aus den Modellen aller Server. Welche Version läuft, steht oben
in der Web-UI (z. B. `v1.0.1`, Commit im Tooltip) und in der ersten Log-Zeile; Änderungen stehen in `CHANGELOG.md`.

Jeder Server wird pro Anfrage höchstens einmal versucht. Als Fehler gelten Verbindungsfehler, **jeder Statuscode ab 400**
(auch 400 „Kontext zu groß“ – ein Server mit mehr Kontext kann es schaffen) und **Antworten mit HTTP 200, deren Anfang ein
Fehler ist** (llama.cpp schickt ein SSE-Event `{"error":…}`, Ollama eine Fehlerzeile) oder die leer sind: Der Balancer prüft
den Anfang jeder Antwort, bevor er etwas an den Client schickt – das verzögert nichts, der erste Token geht sofort durch.
Mit `RETRY_STATUS=404,429` (Liste) werden nur 5xx und die genannten Codes wiederholt, andere 4xx sofort durchgereicht. Ein Server, der nur
„belegt“ meldet (503/429), zählt nicht als fehlerhaft und darf dieselbe Anfrage wieder bekommen, sobald er frei ist.
Läuft die Antwort schon (Stream begonnen), ist kein Wechsel mehr möglich. In „Letzte Anfragen“ zeigt `↻ 3×` mit Tooltip
den Weg, z. B. `gpu: 500 → amd: 503 → nuc (Fallback): 200`; fehlgeschlagene Versuche zählen beim jeweiligen Server als Fehler.

### Fallback-Gruppe

Im Dialog einer Gruppe lassen sich eine **Fallback-Gruppe**, eine **Wartezeit** und optional ein **Modell dort** einstellen.
Wartet eine Anfrage dieser Gruppe länger als die Wartezeit in der Queue, dürfen zusätzlich die Server der Fallback-Gruppe
sie übernehmen – ein freier Server der eigenen Gruppe hat dabei immer Vorrang. Sind alle Server der Gruppe down oder
deaktiviert, wird sofort auf die Fallback-Gruppe ausgewichen. Mit „Modell dort“ ersetzt der Balancer das Feld `model`
der Anfrage für den Fallback-Server (z. B. `qwen3-32b` → `llama3.2:3b`), sonst geht der Modellname unverändert durch.
Die Queue wird jede Sekunde neu geprüft. In „Letzte Anfragen“ erscheint so eine Anfrage als `qwen-gross → klein` und
`qwen3-32b → llama3.2:3b`. Der Fallback reicht eine Stufe weit (der Fallback des Fallbacks wird nicht verwendet).

### Prüfung der Tags im generierten Text

Der Balancer liest den Text mit, den das Modell erzeugt (Streams und normale Antworten, OpenAI-, Anthropic- und Ollama-Format),
und prüft, ob Tags korrekt geöffnet und geschlossen werden. Gemeldet werden: nicht geschlossene Tags, schließende Tags ohne
öffnendes, falsche Reihenfolge (`<a><b></a></b>`) und ein noch offenes Tag, das erneut geöffnet wird. Standardmäßig geprüft
werden `think, thinking, reasoning, tool_call, tool_calls, function_call, answer` (`TAG_CHECK`, `*` = alle Tags, `off` = aus);
anderes HTML in Antworten wird ignoriert. Ein `</think>` ohne öffnendes Tag am Anfang der Antwort gilt nicht als Fehler,
weil Chat-Templates von Reasoning-Modellen (Qwen3, DeepSeek-R1) das `<think>` in den Prompt schreiben. Bricht ein Stream
vorzeitig ab, werden offene Tags nicht gemeldet. Fehler stehen in „Letzte Anfragen“ (Filter „nur Antworten mit Tag-Fehlern“),
als Zähler auf der Server-Karte und in der Statistik pro Server und Modell. Der Inhalt wird nicht gespeichert.

### Tokens pro Sekunde

Pro Anfrage: Antwort-Tokens geteilt durch die Zeit vom ersten Byte bis zum Ende (Generierung) und Prompt-Tokens geteilt durch
die Zeit bis zum ersten Byte (Prompt-Verarbeitung) – nur bei Streams mit Token-Angaben. Die Server-Karte zeigt den
Durchschnitt, den letzten Wert und die Prompt-Geschwindigkeit; laufende Anfragen zeigen einen Live-Schätzwert
(≈ ein Token pro gestreamtem Chunk). Die Statistik zeigt Ø tok/s pro Server, Modell und Tag.

### Anfragen stoppen

Der Knopf **■ Stop** auf der Server-Karte (bzw. ■ in der Listenansicht) bricht die laufende Anfrage dieses Servers nach
einer Rückfrage sofort ab. Der Balancer schließt dabei die Verbindung zum LLM-Server – llama.cpp, Ollama und vLLM beenden
die Generierung dann von selbst – und der Slot wird sofort frei für die nächste Anfrage aus der Warteschlange.
Der Client bekommt eine Fehlermeldung: läuft der Stream schon, ein letztes SSE-Event
`{"error":{"message":"llm-balancer: request stopped by administrator"}}`, sonst HTTP 499 mit derselben Meldung
(499 statt 5xx, damit OpenAI-Clients und LiteLLM die Anfrage nicht automatisch wiederholen). Gestoppte Anfragen zählen als
„abgebrochen“, nicht als Fehler des Servers. Per API: `POST /balancer/api/backends/{name}/stop` (alle laufenden Anfragen
des Servers) oder `POST /balancer/api/requests/{id}/stop` (eine Anfrage, auch wartende).

**„Ein“ und „Aus“ immer aus Sicht dessen, um den es geht:** Bei API-Tokens aus Sicht der Anwendung – *Aus* = Prompt,
den sie sendet, *Ein* = Antwort, die sie bekommt. Bei Servern (und Modellen) aus Sicht des Servers – *Ein* = Prompt, den
er bekommt, *Aus* = Antwort, die er erzeugt. Jede Spalte nennt zusätzlich „Prompt“ bzw. „Antwort“; der CSV-Export heißt
`Tokens_Prompt` / `Tokens_Antwort`.

### Kostendeckel

* **API-Token** (Tab API-Zugriff → Bearbeiten): Deckel für die eingehenden Kosten pro Tag, Monat oder gesamt. Ist er
  erreicht, lehnt der Balancer Anfragen mit dem Token mit HTTP 429 (`insufficient_quota`, wie OpenAI bei aufgebrauchtem
  Guthaben) ab. Im nächsten Tag/Monat oder nach Erhöhen des Deckels ist der Token automatisch wieder frei. Zusätzlich
  kann ein Token mit „Sperren“ manuell abgeschaltet werden (HTTP 403).
* **Server** (Server-Dialog): Deckel für die ausgehenden Kosten. Ist er erreicht, bekommt der Server keine Anfragen mehr
  (Zustand „Kostendeckel“) und Anfragen gehen an die übrigen Server bzw. die Fallback-Gruppe – auch solche, die schon
  warten. Laufende Anfragen werden zu Ende bearbeitet; dadurch kann der Deckel um die letzte Anfrage überschritten werden.

Die Kosten pro Tag/Monat kommen aus der Tagesstatistik (Zeitzone `TZ`); „gesamt“ bei Servern umfasst den
Statistikzeitraum (`STATS_DAYS`), bei Tokens die „Einnahmen gesamt“. „Statistik löschen“ setzt damit auch die Deckel zurück.

### Report und E-Mail-Benachrichtigung

Der Tab **Report** zeigt alle Ereignisse (Server nicht erreichbar / liefert Fehler / wieder da, Kostendeckel erreicht /
wieder frei) und enthält die Einstellungen für E-Mails: SMTP-Server, Port, Verschlüsselung (STARTTLS, SSL/TLS, keine),
Anmeldung, Absender, Empfänger, welche Ereignisse eine Mail auslösen, und nach wie vielen Sekunden eine Störung gemeldet
wird – kurze Aussetzer lösen so keine Mail aus. Pro Störung gibt es eine Mail und eine, wenn sie vorbei ist. Mit
„Testmail senden“ lässt sich die Verbindung prüfen. Gespeichert wird in `/data/notify.json` (inkl. Passwort, Dateirechte
0600; das Passwort wird nie an die UI zurückgegeben), das Protokoll in `/data/events.json` (letzte 300 Ereignisse).
Der Container muss den SMTP-Server erreichen können (Port 587/465/25).

### Eingehende und ausgehende Preise

* **Eingehend (API-Tokens):** was ein Client pro 1 Mio. Tokens bezahlt – Tab API-Zugriff → „Bearbeiten“.
* **Ausgehend (Server):** was ein Server pro 1 Mio. Tokens kostet (Strom, Hardware, bezahlter Anbieter) –
  im Dialog „Bearbeiten“ des Servers.

Pro Anfrage werden beide Beträge berechnet („Einnahme“ und „Kosten Server“ in Letzte Anfragen). Die Statistik zeigt pro
Server, Modell und Tag **Einnahmen**, **Kosten Server** und den **Saldo**; der CSV-Export enthält beide Spalten.

### Token-Zählung und Preise

Der Balancer liest die Token-Angaben aus den Antworten: `usage` (OpenAI, auch im letzten Stream-Chunk), `input_tokens` /
`output_tokens` (Anthropic), `prompt_eval_count` / `eval_count` (Ollama nativ) und `timings` (llama.cpp). Damit
OpenAI-kompatible Server auch bei Streams Zahlen liefern, ergänzt der Balancer Stream-Anfragen an `…/completions` um
`"stream_options": {"include_usage": true}`, sofern der Client das nicht selbst setzt (abschaltbar mit `STREAM_USAGE=false`;
der Client erhält dann einen zusätzlichen letzten Chunk mit `usage` und leerem `choices`).

Pro API-Token lässt sich unter **API-Zugriff → Bearbeiten** ein Preis pro 1 Mio. Prompt- und Antwort-Tokens hinterlegen
(Währung über `CURRENCY`). Die Kosten werden beim Abschluss jeder Anfrage mit dem dann gültigen Preis berechnet; eine
Preisänderung rechnet bereits gezählte Kosten nicht um. Die Statistik zeigt Tokens pro Server und Tag sowie Anfragen,
Tokens und Kosten pro API-Token im gewählten Zeitraum; „Letzte Anfragen“ zeigt Tokens, Tokens/s und Kosten jeder Anfrage
(die letzten 300, davon 15 sichtbar, der Rest per Scrollen).

### Tokens

**Zugriff auf den Balancer (Clients → Balancer).** Sobald mindestens ein Token existiert, brauchen alle Anfragen an die
LLM-API (alles außerhalb von `/balancer/`) einen gültigen Token als `Authorization: Bearer <token>`, `x-api-key` oder `api-key`.
Sonst antwortet der Balancer mit 401 im OpenAI-Fehlerformat. Tokens kommen aus `API_KEYS` (fest, nur per Umgebungsvariable
änderbar) oder werden in der UI unter **API-Zugriff** erzeugt. Ein erzeugter Token wird genau einmal angezeigt; gespeichert
wird nur sein SHA-256-Hash in `/data/access.json`. Die UI zeigt pro Token „zuletzt benutzt“ und die Anzahl Anfragen, in
„Letzte Anfragen“ steht der Token-Name als Client. Ohne jeden Token ist die API offen (die UI weist darauf hin).

**Tokens der LLM-Server (Balancer → Server).** Pro Server kann ein Bearer-Token hinterlegt werden (Dialog „Bearbeiten“
oder `BACKEND_KEYS="gpu=sk-…"` als Startwert). Er wird als `Authorization: Bearer …` gesendet, auch beim Health-Check.
Die Zugangsdaten des Clients werden nie an die Server weitergegeben, sobald der Balancer selbst Tokens prüft oder der Server
einen eigenen Token hat. Lehnt ein Server den Token ab (401/403), wird er als *fehlerhaft* bzw. beim Health-Check als *down*
mit entsprechendem Hinweis angezeigt. Server-Tokens verlassen den Balancer nie über API oder UI und liegen in
`/data/backends.json` (Dateirechte 0600).

### Alle Optionen

Entweder per Umgebungsvariablen oder per `config.json` (Pfad über `CONFIG`, sonst `./config.json` bzw.
`/config/config.json`). Umgebungsvariablen überschreiben die Datei. Siehe `config.example.json`.

| Variable / Feld | Standard | Bedeutung |
|---|---|---|
| `BACKENDS` / `backends` | – | Startkonfiguration: `name=url*maxParallel@priorität`, kommagetrennt. Name, `*N` und `@N` sind optional. Darf leer sein, wenn alles in der UI eingetragen wird |
| `DATA_DIR` / `data_dir` | `data` (Docker: `/data`) | Ablage für die in der UI geänderte Server-Liste |
| `STATS_DAYS` / `stats_days` | `400` | Aufbewahrung der Tagesstatistik |
| `TZ` | UTC | Zeitzone für die Tagesgrenzen der Statistik, z. B. `Europe/Berlin` |
| `API_KEYS` / `api_keys` | – | Tokens für Clients der LLM-API: `name=token,…` (oder nur `token,…`) |
| `BACKEND_KEYS` | – | Startwerte für die Bearer-Tokens der Server: `name=token,…` |
| `UNKNOWN_MODEL` / `unknown_model` | `all` | `reject`: Modelle, die keine Gruppe und kein Server kennt, mit 404 ablehnen (nur wenn Gruppen existieren) |
| `STREAM_USAGE` / `stream_usage` | `true` | Stream-Anfragen um `stream_options.include_usage` ergänzen, damit Tokens gezählt werden |
| `CURRENCY` / `currency` | `€` | Währung der Preise |
| `RETRY_STATUS` / `retry_status` | `all` | `all`: jeder Fehler ab 400 wird auf anderen Servern wiederholt; Liste (z. B. `404,429`): nur 5xx und diese Codes |
| `TAG_CHECK` / `tag_check` | `think,thinking,reasoning,tool_call,tool_calls,function_call,answer` | Geprüfte Tags im generierten Text; `*` = alle, `off` = aus |
| `DNS_SERVERS` / `dns_servers` | – | DNS-Server (IPs, optional mit Port) für die Namen der LLM-Server; danach Fallback auf den Container-Resolver |
| `ADMIN_TOKEN` / `admin_token` | – | Wenn gesetzt, sind Änderungen in UI/API nur mit diesem Token möglich (Header `X-Admin-Token`) |
| `LISTEN` / `listen` | `:8080` | Listen-Adresse |
| `QUEUE_TIMEOUT_SEC` / `queue_timeout_sec` | `900` | Max. Wartezeit in der Queue (0 = unbegrenzt), danach HTTP 504 |
| `MAX_QUEUE` / `max_queue` | `0` | Max. Länge der Queue (0 = unbegrenzt), danach HTTP 503 |
| `HEALTH_PATH` / `health_path` | `/v1/models` | Pfad für Health-Checks; leer = deaktiviert (dann 10 s Sperre nach Verbindungsfehler). Für llama.cpp passt auch `/health` |
| `HEALTH_INTERVAL_SEC` / `health_interval_sec` | `15` | Intervall der Health-Checks |
| `slot_methods` | `["POST"]` | HTTP-Methoden, die einen Slot belegen |
| `retry_status` | `[503]` | Upstream-Statuscodes, bei denen auf einen anderen Server ausgewichen wird |
| `ui_prefix` | `/balancer` | Pfad der Web-UI/API (alles andere wird durchgereicht) |

Beispiel mit einem Server, der 2 parallele Anfragen kann (z. B. llama.cpp mit `-np 2`), und Bevorzugung der GPU:

```
BACKENDS="gpu=http://10.0.0.5:8080*2@0,amd=http://10.0.0.6:8080@1"
```

## LiteLLM anbinden

Statt mehrerer Deployments mit Zufallsverteilung nur noch **ein** Deployment, das auf den Balancer zeigt:

```yaml
model_list:
  - model_name: local-llm
    litellm_params:
      model: openai/qwen3          # Modellname wird 1:1 an den Backend-Server weitergegeben
      api_base: http://llm-balancer:8080/v1
      api_key: "llmb-…"            # Token aus API_KEYS bzw. „API-Zugriff“ in der UI ("none", wenn keiner eingerichtet ist)
      timeout: 1800                # Queue-Wartezeit + Generierungsdauer einplanen
      stream_timeout: 1800
```

Hinweise:

* Der Modellname wird nicht umgeschrieben. Die Backends müssen den Namen also entweder alle kennen
  (Ollama: gleiches Modell-Tag) oder ihn ignorieren (llama.cpp-Server mit einem geladenen Modell).
* LiteLLM-Timeouts und Retries so wählen, dass Anfragen nicht abgebrochen werden, während sie in der Queue warten –
  sonst schickt LiteLLM Duplikate hinterher. Ein vom Client abgebrochener Request wird sauber aus der Queue entfernt
  bzw. sein Slot sofort freigegeben.
* Jede Antwort enthält den Header `X-LLM-Balancer-Backend` mit dem verwendeten Server.

## Web-UI

`http://<host>:8080/balancer/` – aktualisiert sich jede Sekunde. Status pro Server: *frei*, *belegt*,
*nicht erreichbar*, *deaktiviert*. Pro Server: **Bearbeiten**, **Deaktivieren (drain)** – nimmt ihn aus der
Verteilung, laufende Anfragen laufen zu Ende, praktisch für Modellwechsel oder Wartung – und **Entfernen**.
Oben rechts: **+ Server hinzufügen**.

Die Status-Ansicht ist ohne Anmeldung sichtbar; für Änderungen `ADMIN_TOKEN` setzen.

### API

| Methode | Pfad | |
|---|---|---|
| GET | `/balancer/api/status` | kompletter Status |
| GET | `/balancer/api/backends` | aktuelle Server-Liste |
| POST | `/balancer/api/backends` | Server hinzufügen, Body `{"name","url","max_concurrent","priority","enabled","api_key"}` |
| PUT | `/balancer/api/backends/{name}` | Server ändern (gleicher Body, Umbenennen erlaubt; `api_key` weglassen = behalten, `""` = entfernen) |
| DELETE | `/balancer/api/backends/{name}` | Server entfernen |
| POST | `/balancer/api/backends/{name}/enable` bzw. `/disable` | aktivieren / drain |
| POST | `/balancer/api/backends/{name}/stop` | laufende Anfragen des Servers abbrechen |
| POST | `/balancer/api/requests/{id}/stop` | eine Anfrage (laufend oder wartend) abbrechen |
| POST | `/balancer/api/reset` | zurück zur Startkonfiguration |
| GET/POST | `/balancer/api/tokens` | Client-Tokens auflisten / erzeugen (`{"name","token"?}`) |
| PUT | `/balancer/api/tokens/{name}` | Preis, Deckel, Sperre: `{"price_in":0.4,"price_out":1.6,"cost_cap":10,"cap_period":"month","disabled":false}` |
| DELETE | `/balancer/api/tokens/{name}` | Client-Token löschen |
| POST | `/balancer/api/groups` | Gruppe anlegen: `{"name","models":[…],"auto_models","members":[…],"force_model","fallback","fallback_after_sec","fallback_model"}` |
| PUT / DELETE | `/balancer/api/groups/{name}` | Gruppe ändern (umbenennen erlaubt; `members` weglassen = unverändert) / löschen |
| GET | `/balancer/api/route?model=…` | Welche Gruppe/Server bekämen dieses Modell? |
| GET/PUT | `/balancer/api/notify` | E-Mail-Einstellungen (Passwort-Feld weglassen = beibehalten) |
| POST | `/balancer/api/notify/test` | Testmail senden |
| GET | `/balancer/api/events` | Ereignisprotokoll |
| GET | `/balancer/api/recent?after=<seq>` | Letzte Anfragen (max. 300), nur neuere als `seq` |
| GET | `/balancer/api/stats?days=30` | Tagesstatistik pro Server, Modell und API-Token (`&format=csv`, `&by=model` für CSV nach Modell) |
| POST | `/balancer/api/stats/reset` | Statistik löschen |

## Fehlersuche

**`lookup <name> on 127.0.0.11:53: server misbehaving` / `no such host`** – Der Name des Servers lässt sich *im Container*
nicht auflösen (127.0.0.11 ist der DNS-Server, den Docker im Container bereitstellt; er fragt die DNS-Server des Hosts).
Häufige Ursachen: der Name ist nur lokal bekannt (Router, Pi-hole, `/etc/hosts` des Hosts, `.local` per mDNS) oder der
Host nutzt `systemd-resolved` (127.0.0.53), das Docker nicht erreicht. Mit http oder https hat das nichts zu tun.
Abhilfe: `DNS_SERVERS` auf den DNS-Server des Netzes setzen (z. B. `DNS_SERVERS: "192.168.1.1"`, mehrere kommagetrennt).
Der Balancer fragt dann zuerst diese Server und erst danach den Docker-DNS, Namen anderer Container funktionieren also weiter.
Alternativ im Stack `dns:` (gilt für den ganzen Container) oder `extra_hosts: ["name:IP"]`.
Prüfen lässt sich die Auflösung so (gleiches Netz wie der Balancer):
`docker run --rm --network <netz-des-stacks> busybox nslookup <name>`.

**`certificate signed by unknown authority`** – Der Server nutzt ein selbstsigniertes Zertifikat oder eines einer eigenen CA.
Beim Server „TLS-Zertifikat nicht prüfen“ aktivieren. (Öffentliche Zertifikate, z. B. Let's Encrypt, werden geprüft und
funktionieren ohne die Option.)

**`server gave HTTP response to HTTPS client`** – Der Server spricht http: URL mit `http://` eintragen.

## Entwicklung

```bash
go test -race ./...
```

Die Tests simulieren langsame Ein-Slot-Server und prüfen u. a. Verteilung, Queue, ungepuffertes Streaming,
Failover bei toten Servern und bei 503, Abbruch in der Queue ohne Slot-Leck sowie Drain.
