# Designförslag: tenancy

Svenska · [English](tenancy.md)

> **Status: förslag, ingen kod än.** Det här dokumentet beskriver en design och de beslut den kräver. Storleksangivelserna är grova (S = dagar, M = några veckor, L = mer än en månad, för en utvecklare).

## 1. Sammanfattning

Lumen skiljer redan kunder åt med *tenants*. Det här förslaget lägger till det som båda våra köpare behöver ovanpå: en **operatörsnivå ovanför tenants** (de som driver Lumen), **tenant-poster** med gränser och livscykel, **användningsmätning**, **lagringstid och branding per tenant** och, senare, **dedikerad lagring** för tenants som kräver hård isolering.

Rekommendationen i en mening: **behåll en platt lista med tenants och lägg ett operatörslager ovanför**, och lämna plats för en förälder/barn-relation mellan tenants utan att bygga den ännu.

## 2. Vem behöver vad

| | Tjänsteleverantörer | Plattformsteam i större företag |
|---|---|---|
| En tenant är | en kund | ett team eller en verksamhet |
| En operatör är | leverantörens egen personal | det centrala plattformsteamet |
| Behöver först | kvoter, användning för fakturering, white-label, kundrapporter | granskning, SSO, central styrning, kostnadsfördelning |
| Behöver senare | dedikerad lagring för stora kunder | dedikerad lagring för reglerade verksamheter |

Samma byggstenar räcker för båda: tenant, operatör, kvot, användning, granskning.

## 3. Hur tenancy fungerar i dag

- En **tenant** är en sträng. Den finns för att användare, API-nycklar, grupper, dashboards, hostar och instanser bär den. Det finns ingen tenant-post.
- Tenant för en förfrågan kommer **bara från den autentiserade identiteten** (användarens tenant eller API-nyckelns tenant). En förfrågan kan inte ange en annan tenant.
- **ClickHouse:** varje rad med spans, loggar och mätvärden har kolumnen `tenant`, och den är första kolumnen i varje tabells sorteringsnyckel. Varje fråga byggs med ett tenantfilter, och tester kontrollerar att en annan tenant inte får något.
- **Elasticsearch (eller JSON-filen):** ett gemensamt index per samling. Dokument bär `tenant`, och läsningar filtrerar på den.
- **Backuper** skrivs per tenant och dag, i separata kataloger. Konfigurationsdumpen innehåller alla tenants och är bara för operatören.
- **Branding** (namn och logga) gäller hela installationen.
- **Lagringstid** är en inställning för hela installationen.
- **Kvoter och mätning:** inga. **Skapande av tenant:** implicit, via kommandoraden (`users add --tenant`, `keys create --tenant`) eller den första administratören från miljön.
- Det finns **inget operatörsbegrepp**: varje administratör är administratör för en tenant.

## 4. Krav

| # | Krav | Båda köparna? |
|---|---|---|
| R1 | Den som driver Lumen kan se och hantera alla tenants, utan att av misstag kunna läsa deras data | Ja |
| R2 | Skapa, stänga av, återuppta och avsluta en tenant, inklusive fullständig rensning av dess data | Ja |
| R3 | Gränser per tenant (hostar, användare, insamlingstakt, lagrad volym) med tydliga fel och varningar | Ja |
| R4 | Användning per tenant och dag, exporterbar, lämplig för fakturering eller kostnadsfördelning | Ja |
| R5 | Lagringstid som kan skilja sig per tenant | Ja |
| R6 | Branding och inloggningsadress per tenant (white-label) | Leverantörer |
| R7 | Supportåtkomst: operatören kan titta in i en tenant, bara med spår och helst med tenantens medgivande | Ja |
| R8 | Hård isolering på begäran: separat lagring för en tenant | Stora kunder |
| R9 | En tenant kan inte sakta ner de andra (bullrig granne) | Ja |
| R10 | En installation med en tenant ser inget av detta och för den ändras ingenting | Ja |

## 5. Alternativ för modellen

| | A. Platta tenants plus ett operatörslager | B. Hierarkiska tenants (organisation, verksamheter) | C. Arbetsytor inuti en tenant |
|---|---|---|---|
| Vad det är | Operatör, sedan tenants, sedan användare | Tenants kan ha underordnade tenants som ärver gränser och policyer | En tenant innehåller projekt med egna dashboards och behörigheter |
| Passar leverantörer | Ja | Delvis (återförsäljare) | Nej |
| Passar företag | Mestadels | Ja | Ja |
| Kostnad | Liten | Stor: arv av kvoter, behörigheter, branding | Medel: ett nytt scope på varje objekt |
| Risk | Företag kan vilja ha underenheter senare | Komplexitet innan behovet är bevisat | Dubblerar det grupper redan gör |

**Rekommendation: A, med en krok för B.** Ge varje tenant-post ett valfritt fält `parent` som är tomt och oanvänt tills vidare. Behöver en kund underenheter lägger vi till dem då. Alternativ C behövs inte: grupper och dashboards täcker det mesta.

## 6. Designen

### 6.1 Tenant-posten

En ny samling `tenants` med en post per tenant: id (en kort slug som används i URL:er och lagring), visningsnamn, status (`active`, `suspended`, `offboarding`), skapad och senast aktiv, kontakt, anteckningar, kvoter (6.4), avvikande lagringstid (6.6), branding (6.7), domäner (6.7), `support_access` (`off`, `ask`, `allow`) och det tomma `parent`.

**Migrering:** vid start får varje tenant-sträng som redan finns på användare, nycklar eller dokument en post med standardvärden. Det är idempotent, så att köra det två gånger ändrar ingenting.

### 6.2 Operatören

- **Operatörer är användare i en reserverad tenant** som heter `operator`. De loggar in som alla andra.
- Ett nytt behörighetsområde, `operator`, styr konsolen. Det kan bara ges till grupper i den reserverade tenanten.
- En operatör **kan som standard inte läsa en tenants data**. För att titta in *går* hen in i en tenant: en tidsbegränsad session (standard 60 minuter), skrivskyddad om hen inte uttryckligen ber om skrivrätt, med en synlig banner. Varje inträde och varje åtgärd därinne skrivs till granskningsloggen med operatörens namn.
- När en tenant har ställt in `support_access` på `ask` väntar inträdet på att en tenantadministratör godkänner det. Med `off` nekas inträdet. (Operator-tillägget levererar inställningen. I Community är en operatör den som har installationen, som i dag.)
- I en **installation med en tenant** är operatörslagret vilande: ingen konsol, ingen banner, ingen ändring av beteendet.

### 6.3 Livscykel

| Steg | Vad som händer |
|---|---|
| Skapa | Operatören skapar tenanten och dess första administratör. En tenant skapas med gränserna för en vald *plan* (en namngiven uppsättning kvoter). |
| Stäng av | Inloggning nekas för tenantens användare med ett tydligt meddelande. Inkommande data **avvisas** med ett tydligt fel (standard) eller tas emot och kastas (ett alternativ), så att kundens agenter inte fyller sina diskar. Redan lagrad data behålls. |
| Återuppta | Allt fungerar direkt igen. |
| Avsluta | Först erbjuds en export (backupfiler och en JSON-dump av dokument). Därefter körs en rensning: ClickHouse-rader raderas per tenant, dokument raderas, backupkataloger tas bort och nycklar återkallas. En rensningsrapport listar vad som togs bort. Granskningsposten om avslutet behålls. |

### 6.4 Kvoter och gränser

| Gräns | Räknas som | Upprätthålls |
|---|---|---|
| Hostar | olika hostar som rapporterat de senaste 24 timmarna | varna; hård gräns nekar nya hostar |
| Instanser, användare, grupper, nycklar, dashboards | antal objekt | skapandet misslyckas med ett meddelande som namnger gränsen |
| Insamlingstakt | accepterade poster per sekund och tenant | servern svarar **429 med `Retry-After`**; OTLP-klienter försöker igen |
| Daglig insamlingsvolym | accepterade byte per dag | 429 efter gränsen; varna vid 80 procent |
| Lagrad volym | rader eller byte i lagringen, mäts periodiskt | varna; valfritt neka insamling |
| Frågor | samtidiga frågor och största tidsintervall | 429 eller 400 med ett meddelande |

Gränser är som standard *mjuka* (varningar) och *hårda* när operatören väljer det. En tenantadministratör ser sina egna gränser och sin användning på en sida. Operatören ser alla tenants i konsolen.

### 6.5 Användningsmätning

- Servern räknar accepterade poster och byte per tenant och signal i minnet och skriver dem en gång i minuten till en ClickHouse-tabell `usage_events`, som summeras dagligen till `usage_daily`: tenant, dag, signal, poster, byte, olika hostar, instanser, frågor.
- Export som CSV eller JSON via API:t, för operatörens egen fakturering. Lumen gör **inte** fakturering.
- Mätningen är tillräckligt exakt som *vägledning* för fakturering. Definitionen ("accepterade OTLP-nyttolastbyte") är dokumenterad, så att kunder kan kontrollera den.

### 6.6 Lagringstid per tenant

I dag förfaller hela tabellen enligt en regel. Tre sätt att skilja per tenant:

| Alternativ | Hur | Avvägning |
|---|---|---|
| **A. Förfallokolumn** | En ny kolumn `expires` sätts vid insättning från tenantens lagringstid, och tabellens regel blir "radera när `expires` har passerat" | En regel för alla tenants; en engångsfyllning av gamla rader behövs. **Rekommenderas.** |
| B. Flera villkorliga regler | En förfalloregel per tenant, var och en med ett villkor på `tenant` | Ändrar tabelldefinitionen varje gång en tenant ändras |
| C. Schemalagda raderingar | Ett jobb raderar gamla rader per tenant | Tunga raderingar; långsammare att frigöra utrymme |

Backuper följer också tenantens egen inställning: backuplagringstid per tenant, eftersom backuper redan är per tenant.

### 6.7 Branding och inloggningsadress

- Namn och logga blir per tenant. Inloggningssidan känner inte till tenanten före inloggning, så tenanten väljs via **värdnamn**: `kund.example.com` pekar på en tenant, och inloggningssidan visar den tenantens branding. Det nakna värdnamnet tillhör en standardtenant.
- På en tenants värdnamn är inloggning **begränsad till den tenantens användare** (och operatörer). Det hindrar också att någon kan ta reda på vilka tenants som finns.
- TLS för egna domäner ligger kvar hos reverse proxyn, som i dag. Lumen mappar bara värdnamnet.
- En tenants logga serveras med samma begränsningar som i dag (verifierad på innehåll, inga skript).

### 6.8 Isolering

Isoleringen förblir **logisk** (gemensamma tabeller, tenantfilter) för alla, och skyddas i lager:

1. Tenanten kommer bara från identiteten, aldrig från en förfrågansparameter.
2. Frågebyggarna lägger själva till tenantfiltret; en databasfråga utan det går inte att bygga genom dem.
3. Tester: varje ny endpoint och varje ny fråga får ett test där en andra tenant inte ser något. En gemensam testhjälp gör det billigt att skriva.
4. Fuzz- eller egenskapstester på frågebyggarna och dokumentfiltren.
5. Gränser per fråga (tid, minne) mot bullriga grannar (R9).

**Dedikerad lagring** (R8) är ett senare alternativ: en tenant mappas till en *lagringsprofil* (ClickHouse-databas eller -server, Elasticsearch-indexprefix, backupkatalog). Allt som talar med lagringen slår upp profilen via tenanten. Det kostar verkligt driftarbete (migreringar, backuper och uppgraderingar per profil), så det är sista fasen och en tilläggsfunktion. Radpolicyer på databasnivå i ClickHouse övervägdes och avvisades tills vidare: de kräver en databasanvändare per tenant.

### 6.9 API och skärmar

- `GET/POST /api/v1/operator/tenants`, `GET/PUT/DELETE /api/v1/operator/tenants/{id}`, med åtgärderna `suspend`, `resume` och `offboard`.
- `GET /api/v1/operator/usage?from=&to=&tenant=` (JSON eller CSV), `GET /api/v1/usage` för en tenants egen användning.
- `POST /api/v1/operator/tenants/{id}/enter` och `/leave`.
- Operatörsskärmar: *Tenants* (status, hostar, instanser, användare, insamlingstakt, lagrad volym, senaste aktivitet, varningar), en tenantsida (gränser, användningsdiagram, användare, domäner, stäng av, gå in) och *Användning*.
- Tenantskärmar: *Användning och gränser* under Inställningar.

### 6.10 Behörigheter

Ett nytt område, `operator`, med de vanliga nivåerna ingen, läs och skriv, som bara kan ges i den reserverade tenanten. En tenants egna gränser och användning kan läsas med den befintliga Settings-behörigheten.

## 7. Migrering från koden som den ser ut nu

1. Skapa tenant-poster för befintliga tenants (idempotent) vid start.
2. Lägg till området `operator` och den reserverade tenanten. Inget syns förrän en operatör finns.
3. Befintliga administratörer förblir vad de är. Den första administratören som skapas via miljön i en **installation med en tenant** blir inte operatör.
4. Lagringstid per tenant (6.6) kräver kolumnen `expires` och en fyllning. Det är den enda ändring som rör lagrad data. Kör den som ett uttryckligt, återupptagbart steg med förloppsvisning.
5. Allt annat lägger till nya samlingar och nya fält med standardvärden.

## 8. Säkerhetsöverväganden

- **Operatörskonton är de mest värdefulla kontona.** Rekommendera och (i Enterprise) kräv MFA för dem, en kort sessionstid och granskning av varje operatörsåtgärd.
- **Uppräkning:** inloggning på en tenants värdnamn får inte avslöja om en användare finns i en annan tenant.
- **Kvotkringgående:** gränser kontrolleras på servern och kan inte påverkas av en klient.
- **Personifiering:** en session som gått in kan inte ändra lösenord, nycklar eller tenantens egna administratörer om inte skrivrätt begärts, och de syns alltid i granskningsloggen.
- **Rensning måste verifieras:** efter ett avslut bekräftar en kontroll att inga rader, dokument eller filer finns kvar för tenanten.

## 9. Vilken utgåva

| Utgåva | Innehåll |
|---|---|
| Community | Isolering mellan tenants (alltid), implicita tenants och kommandoraden som i dag, granskningslogg över känsliga åtgärder |
| Operator-tillägg | Tenant-konsol och livscykel, kvoter, användningsmätning och export, branding, domäner och lagringstid per tenant, supportåtkomst med medgivande, dedikerad lagring |
| Enterprise | Påtvingad MFA för operatörer, SAML per tenant, export av granskningslogg |

Isolering mellan tenants flyttas aldrig till en betald utgåva (se utgåvostadgan).

## 10. Faser

| Fas | Innehåll | Storlek |
|---|---|---|
| 1 | Tenant-poster, operatörslagret och inträde i en tenant, granskningslogg, livscykel (skapa, stäng av, återuppta) | M |
| 2 | Kvoter och användningsmätning med export, tenantens egen användningssida | M |
| 3 | Lagringstid per tenant (kolumnen `expires`), branding per tenant och mappning av värdnamn, avslut med rensning | M till L |
| 4 | Dedikerade lagringsprofiler | L |

## 11. Beslut som behövs

1. Operatörsmodell: platta tenants plus ett operatörslager (rekommenderas) eller hierarkiska tenants.
2. Namnet på den reserverade tenanten för operatörer, och om operatörer får finnas i en installation med en tenant.
3. Avstängning: avvisa inkommande data (rekommenderas) eller ta emot och kasta.
4. Angreppssätt för lagringstid: kolumnen `expires` (rekommenderas) eller något av alternativen.
5. Om tenantens egen användningssida är Community eller del av Operator-tillägget.
6. Hur stark isoleringen måste vara: bara logisk, eller dedikerad lagring för vissa tenants.

## 12. Hur vi testar det

- Ett gemensamt test över tenants: varje endpoint och fråga anropas som tenant A med tenant B:s identifierare, och måste ge inget eller nekas.
- Kvottester med en låtsasklocka: gränser, varningar, `Retry-After` och nollställning vid midnatt.
- Migreringstester på en kopia av realistiskt formad data, inklusive att köra migreringen två gånger.
- Avslutstest: efter en rensning finns ingen rad, inget dokument, ingen fil och ingen nyckel kvar.
- Ett belastningstest med en bullrig tenant bredvid en tyst, för att kontrollera frågegränser och 429-beteende.
