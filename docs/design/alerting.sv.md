# Designförslag: larm

Svenska · [English](alerting.md)

> **Status: förslag, ingen kod än.** Det här dokumentet beskriver en design och de beslut den kräver. Storleksangivelserna är grova (S = dagar, M = några veckor, L = mer än en månad, för en utvecklare).

> **Genomförandestatus (oktober 2026).** Fas 1 och 2 är byggda, plus Enterprise-notifierarna ur fas 3. Skillnader mot förslaget: *dirigeringar* är matchare på kanalen (allvarlighetsgrad och etiketter) i stället för ett eget objekt. Larmhistorik och leveransloggen ligger i dokumentlagringen (historiken beskärs till de senaste 1000 per tenant) i stället för i en ClickHouse-tabell. Meddelanden har en fast layout, inte mallar. Trace-regler, eskalering och jour, underhållsfönster, tilldelning, interaktiva chattåtgärder, SLO:er och motorns egna mätvärden är inte byggda än.

## 1. Sammanfattning

Lumen visar vad som är fel men kan ännu inte **säga till någon**. Det här förslaget lägger till en larmmotor i den öppna kärnan, med e-post, webhook, Slack och Teams, och definierar den förlängningspunkt där Enterprise lägger till Jira, ServiceNow, PagerDuty, Opsgenie, eskalering och jour.

Principen: **kärnan utvärderar och levererar. Enterprise lägger till vem som väcks och vilket system som får ett ärende.**

## 2. Mål och icke-mål

**Mål**
- Larma på allt ett diagram kan visa: ett mätvärde, upp/ner-status, loggrader, traces med fel och latens.
- Larma från de skärmar folk redan använder ("skapa larm från det här diagrammet").
- Aldrig tappa ett larm i tysthet, och aldrig översvämma folk: gruppering, gränser för upprepning, tystningar.
- Vara säkert som standard: servern gör utgående anrop åt användare, så det här får inte bli en väg in i det interna nätet.
- Förbli en enda binär som bara använder Gos standardbibliotek.

**Icke-mål (tills vidare)**
- Avvikelsedetektering och prognoser (Enterprise, senare).
- En produkt för incidenthantering. Lumen lämnar över larm till verktygen som gör det.
- Notiser direkt till mobilappar.

## 3. Vad som finns i dag och kan återanvändas

- **Frågemodellen** för dashboardpaneler (mätvärde, aggregering, filter, gruppering, intervall) och de lagringsmetoder som kör den.
- **Upp/ner-beräkningen** (hostar, tjänster, containrar, Nextcloud-instanser) och frågan efter senaste värde.
- Ett sätt att **lagra hemligheter** krypterat (används för Nextcloud-token) och dokumentlagringen med samlingar per tenant.
- Behörigheter per område och en `edition`-söm för att ersätta beteende.
- Backupjobbets konfigurationsdump, som kommer att behöva de nya samlingarna.

Det som **inte** finns: en schemaläggare, utgående e-post eller HTTP från servern, larmtillstånd och någon larmhistorik.

## 4. Begrepp

| Term | Betydelse |
|---|---|
| **Regel** | Ett villkor, hur ofta det kontrolleras, hur länge det måste gälla, en allvarlighetsgrad och etiketter |
| **Larm** | En utlöst förekomst av en regel för en uppsättning etiketter (till exempel "disk över 90 procent på web1 vid `/data`") |
| **Tillstånd** | `ok`, `pending` (villkoret uppfyllt, men inte tillräckligt länge), `firing`, `resolved` |
| **Kanal** | En mottagare: en lista med e-postadresser, en webhook, en Slack-kanal |
| **Dirigering** | Vilka larm som går till vilka kanaler, efter allvarlighetsgrad och etiketter |
| **Tystning** | En tidsbegränsad ljudlös period för larm som matchar ett mönster |
| **Underhållsfönster** | En planerad period då matchande larm tystas (Enterprise lägger till återkommande fönster och godkännande) |

## 5. Regler

| Typ | Exempel | Utvärderas med |
|---|---|---|
| **Mätvärde** | CPU över 90 procent i 10 minuter, per host | diagrammets fråga, aggregerad över ett fönster |
| **Status** | Host nere i 2 minuter, Nextcloud-instans nere, tjänst eller container har fallerat | samma upp/ner-beräkning som gränssnittet använder |
| **Ingen data** | En host eller ett mätvärde slutade rapportera | senaste värdet äldre än en gräns |
| **Logg** | Fler än 20 `ERROR`-rader på 5 minuter från en tjänst | en loggräkning över ett fönster |
| **Trace** | Felfrekvens eller 95:e percentilen över en gräns | frågan för trace-serier |

En regel har: ett namn, en typ och dess fråga (lagrad exakt som ett diagram lagrar den), en jämförelse och ett tröskelvärde, ett utvärderingsintervall (standard 60 s, minst 15 s), en `for`-tid, en allvarlighetsgrad (`critical`, `warning`, `info`), extra etiketter, en förklarande text och om den är aktiverad. **"Skapa larm från det här diagrammet"** kopierar diagrammets fråga till en ny regel.

**Bakåttest.** Innan man sparar kan redigeraren spela upp regeln över de senaste 24 timmarna och visa när den *skulle* ha utlösts. Det är den enskilt mest användbara funktionen för att få tröskelvärden rätt, så den levereras med första versionen.

## 6. Utvärdering

- En schemaläggare i servern kör varje regel med dess intervall, med lite slumpmässig förskjutning så att regler inte alla körs på samma sekund.
- Varje utvärdering kör en **tenantfiltrerad fråga** med en tidsgräns och ger noll eller fler *larmförekomster*, var och en identifierad av ett fingeravtryck (regelns id plus dess sorterade etiketter).
- **Tillståndsmaskin:** `ok` blir `pending` när villkoret är uppfyllt. `pending` blir `firing` när det har gällt i `for`. `firing` blir `resolved` efter att villkoret varit falskt under en **återhämtningsperiod** (standard två utvärderingar), så att ett värde som pendlar kring tröskeln inte flimrar.
- **Policy för ingen data** per regel: behandla som OK, behandla som utlöst, eller behåll senaste tillståndet.
- **Fel vid utvärdering** (databasen nere, frågan för långsam) **löser inte** larm. Regeln visar hälsotillståndet *fel*, och ett inbyggt larm berättar att larmfunktionen själv mår dåligt.
- **Omstarter:** tillståndet sparas, så en omstart skickar inte nya notiser för larm som redan var utlösta och tappar inte klockan för ett väntande larm.
- **Gränser** skyddar servern: ett högsta antal regler per tenant, ett lägsta intervall, ett högsta antal serier per utvärdering och en tidsgräns för frågor.
- **En enda utvärderare.** Community-utgåvan kör en server, alltså en utvärderare. Hög tillgänglighet (Enterprise) lägger till ett lån så att bara en server utvärderar åt gången.

## 7. Notiser

### 7.1 Flödet

1. **Dirigera** efter allvarlighetsgrad och etiketter till en eller flera kanaler.
2. **Gruppera** larm som hör ihop (standard: per regel och host), vänta en kort stund (30 s) så att relaterade larm kommer i samma meddelande, och skicka ett meddelande för gruppen.
3. **Upprepa** så länge larmet är utlöst, med ett inställbart intervall (standard 4 timmar). Sluta när det är kvitterat.
4. **Skicka** med en tidsgräns och omförsök med växande väntetid i upp till en timme. En **leveranslogg** noterar varje försök och dess resultat.
5. **Lösningsmeddelande** när gruppen har återhämtat sig.

Leveransfel syns i gränssnittet och utlöser ett inbyggt larm. En kanal som fortsätter att misslyckas markeras som sjuk, så att ingen tror sig vara skyddad när det inte stämmer.

### 7.2 Förlängningspunkten

Kärnan definierar vad en **notifierare** är: ett namn, ett sätt att validera dess inställningar, ett sätt att **skicka** en grupp larm och rapportera lyckat eller misslyckat, och en **testfunktion**. Community registrerar notifierarna nedan. Enterprise lägger till fler genom att registrera dem vid start. Kärnan importerar aldrig Enterprise-kod (se utgåvostadgan).

### 7.3 Kanaler per utgåva

| Kanal | Utgåva |
|---|---|
| **E-post** (SMTP med TLS och autentisering) | Community |
| **Webhook** (JSON, signerad med en hemlighet, egna rubriker) | Community |
| **Slack**, **Microsoft Teams** (inkommande webhooks) | Community |
| **Heartbeat** (ett regelbundet utgående ping till en extern tjänst, så att du märker när Lumen själv ligger nere) | Community |
| **PagerDuty**, **Opsgenie** | Enterprise |
| **Jira**, **ServiceNow** (skapa ett ärende, lägg till kommentarer vid upprepning, stäng det vid återhämtning, minns vilket larm som hör till vilket ärende) | Enterprise |
| **Eskaleringspolicyer och jourscheman** (notifiera A nu, B efter 15 minuter om ingen kvitterat, rotationer, överstyrningar) | Enterprise |
| **Interaktiva chattmeddelanden** (kvittera från Slack eller Teams) | Enterprise |

### 7.4 Meddelanden

Meddelandetexter är mallar över en liten uppsättning fält (regel, etiketter, värde, tillstånd, länk till Lumen). Mallar kan inte anropa funktioner eller läsa något annat, så en mall kan aldrig läcka data eller köra kod. Enterprise lägger till mallar per kanal och ett mallgalleri.

## 8. Kvittera och tysta

- **Kvittera** (Community): markerar ett larm som sett och stoppar upprepade notiser. Tillståndet förblir `firing` tills det återhämtar sig. Vem som kvitterade, och när, noteras. **Tilldelning** till en person är Enterprise.
- **Tysta** (Community): ljudlöst för larm som matchar etiketter, med en sluttid och en anledning, skapat från gränssnittet med två klick (till exempel från ett larm, "tysta i 2 timmar"). Tystade larm utvärderas och visas fortfarande, men skickas inte.
- **Underhållsfönster** (Enterprise): återkommande fönster, godkännande och en kalendervy, per tenant.

## 9. Säkerhet

- **Förfalskade serverförfrågningar.** En webhook-URL får servern att anropa något åt en användare. Som standard **nekar servern adresser som är loopback, länklokala, privata eller adresser till molnleverantörers metadata**, kontrollerat *efter* namnuppslagning och på nytt vid varje omdirigering. En administratör kan tillåta särskilda interna mål. Tidsgränser och gränser för svarsstorlek gäller.
- **Hemligheter** (SMTP-lösenord, token, webhook-hemligheter) krypteras i vila och **visas aldrig mer** i gränssnittet, som för Nextcloud-token. Att ändra en kanal kräver inte att man skriver in en hemlighet som förblir densamma.
- **E-post:** rubriker byggs bara av validerade fält för att förhindra rubrikinjektion, och avsändaradressen är fast per installation eller kanal.
- **Webhook-signering:** varje anrop bär en signatur över innehållet och en tidsstämpel, så att mottagare kan verifiera den och avvisa återuppspelning.
- **Behörigheter:** två nya områden. `alerts` (läs: se regler, larm, tystningar. Skriv: hantera regler och tystningar) och `notifications` (hantera kanaler, som innehåller hemligheter och kan nå nätverket). Den inbyggda gruppen *User* får bara läsrätt på larm.
- **Granskning:** varje ändring av en regel, dirigering, kanal eller tystning noteras med vem och när.
- **Missbruksgränser:** gränser per tenant för regler, kanaler och meddelanden per timme, så att en felaktig regel inte kan skicka tiotusen e-postmeddelanden.

## 10. Tenancy

Regler, kanaler, dirigeringar och tystningar tillhör en tenant, och varje utvärderingsfråga är tenantfiltrerad. Utvärderingen får en rättvis del av servern: en tenants dyra regler kan inte svälta ut de andra. Med Operator-tillägget kan en operatör tillhandahålla **standardkanaler** (till exempel leverantörens egen e-postserver) som tenants får använda utan att se hemligheterna.

## 11. En uppsättning startregler

Levereras som mallar som administratören kan aktivera med ett klick (några aktiverade som standard på en ny installation):

| Regel | Villkor |
|---|---|
| Host nere | en agent har inte rapporterat på 2 minuter |
| Nextcloud-instans nere | instansen är nere i 1 minut |
| Tjänst har fallerat, container nere | en bevakad tjänst eller container körs inte |
| Disken nästan full | en monteringspunkt är över 90 procent |
| Minnestryck | tillgängligt minne under 10 procent i 10 minuter |
| Hög CPU | över 90 procent i 10 minuter |
| Backup saknas | ingen lyckad backup på 36 timmar |
| Loggsökvägar nekade | en agent nekade loggsökvägar från servern |
| Lumen mår dåligt | databasen svarar inte, fel vid insamling, larmleverans misslyckas |
| Disken full om 24 timmar (prognos) | Enterprise |

## 12. Gränssnitt och API

**Skärmar:** *Larm* (vad som är utlöst och väntar, med filter, kvittera, tysta), *Regler* (lista och redigerare med förhandsvisning av frågan och bakåttest), *Kanaler* (med en **Test**-knapp), *Tystningar*, *Historik*. En åtgärd "skapa larm" på diagrampaneler, hostsidor och instanssidor. Startsidan visar hur många larm som är utlösta.

**API (skiss):** `GET/POST/PUT/DELETE /api/v1/alerts/rules`, `POST /api/v1/alerts/rules/{id}/backtest`, `GET /api/v1/alerts` (aktuella), `POST /api/v1/alerts/{id}/ack`, `GET/POST/DELETE /api/v1/alerts/silences`, `GET/POST/PUT/DELETE /api/v1/notifications/channels`, `POST /api/v1/notifications/channels/{id}/test`, `GET /api/v1/alerts/history`.

## 13. Lagring

Nya dokumentsamlingar: regler, dirigeringar, kanaler (med förseglade hemligheter), tystningar och larmens aktuella tillstånd. **Historiken** (tillståndsändringar och leveransförsök) går till en ClickHouse-tabell med en tidsgräns, så att dokumentlagringen inte växer obegränsat. Backupjobbets konfigurationsdump omfattar de nya samlingarna (men aldrig dekrypterade hemligheter).

## 14. Att bevaka bevakaren

Lumen rapporterar sin egen larmfunktion som mätvärden: utvärderingar, utvärderingstid, fel, larm per tillstånd, skickade och misslyckade notiser samt kölängd. **Heartbeat**-kanalen och de inbyggda reglerna "Lumen mår dåligt" täcker fallet där Lumen inte kan berätta det själv.

## 15. Felsituationer

| Situation | Beteende |
|---|---|
| ClickHouse otillgänglig | Utvärderingen ger fel. Larmen behåller sitt tillstånd. Det inbyggda larmet utlöses via kanaler som inte behöver databasen |
| Dokumentlagringen otillgänglig | Servern fortsätter på tillstånd i minnet och försöker skriva igen. Den kastar aldrig utlöst tillstånd |
| SMTP eller en webhook ligger nere | Omförsök med växande väntetid. Leveransloggen visar felet. Kanalen markeras som sjuk |
| En regel är mycket dyr | Tidsgräns för frågan. Regeln markeras som felaktig. Gränser hindrar att den påverkar andra |
| Servern startas om | Tillståndet läses in igen. Larm som redan notifierats skickas inte på nytt |
| Klockan hoppar | Tidsbaserade beslut använder monoton tid där det går. Stora hopp loggas |

## 16. Vilken utgåva

Se avsnitt 7.3 för kanaler. Kort sagt: **Community** har regler, tillstånd, gruppering, gränser för upprepning, kvittering, tystningar, e-post, webhook, Slack, Teams, heartbeat, startreglerna och bakåttest. **Enterprise** lägger till integrationer mot ärendesystem och jour, eskalering, dirigering och mallar i stor skala, återkommande underhållsfönster med godkännande, tilldelning, interaktiva chattåtgärder, SLO:er och prognoser.

## 17. Faser

| Fas | Innehåll | Storlek |
|---|---|---|
| 1 | Motor, mätvärdes- och statusregler, tillståndsmaskin, e-post och webhook, larmlista och regelredigerare, kvittera, enkla tystningar | L |
| 2 | Logg- och trace-regler, Slack och Teams, bakåttest, startregler, historik, självövervakning och heartbeat, "skapa larm" från diagram | M |
| 3 (Enterprise) | PagerDuty, Opsgenie, Jira, ServiceNow, eskalering och jour, dirigering och mallar, underhållsfönster | L |
| 4 (Enterprise) | SLO:er och burn-rate-larm, prognoser, avvikelsedetektering | L |

## 18. Beslut som behövs

1. Lägsta utvärderingsintervall (15 s föreslås) och högsta antal regler per tenant.
2. Var larmhistoriken lagras (en ClickHouse-tabell föreslås).
3. Om e-post behöver en avsändare per tenant, eller en avsändare per installation.
4. Hur man kvitterar: bara i gränssnittet, eller också från en länk i e-postmeddelandet.
5. Om loggbaserade regler ska begränsas, eftersom de är dyrast att utvärdera.
6. Vilka startregler som är aktiverade som standard på en ny installation.

## 19. Hur vi testar det

- **Tillståndsmaskin:** tabelldrivna tester med en låtsasklocka för varje övergång, inklusive flimmer, policyer för ingen data och omstarter.
- **Utvärdering:** en låtsaslagring som returnerar skriptade serier.
- **Notifierare:** kontraktstester mot en lokal HTTP-server och en lokal SMTP-server (omförsök, tidsgränser, omdirigeringar, signatur).
- **Säkerhet:** tester att privata adresser, loopback och metadata-adresser nekas, även via en omdirigering och via namnuppslagning. Tester av mallarnas sandlåda.
- **Från början till slut:** den riktiga agenten och den riktiga servern, en agent som stoppas med flit, och en kontroll att exakt en notis kommer och ett lösningsmeddelande följer.
- **Kaos:** starta om servern medan ett larm är utlöst och ett annat väntar.
