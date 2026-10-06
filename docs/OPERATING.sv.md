# Att driva Lumen för flera tenants (operatörsguide)

Svenska · [English](OPERATING.md)

För dig som driver en installation åt kunder eller team. Designen bakom finns i [design/tenancy.sv.md](design/tenancy.sv.md) och licensen i [LICENSING.sv.md](LICENSING.sv.md).

## Ställ in

1. Installera med `LUMEN_ADMIN_TENANT=operator` i `deploy/.env`. Den första administratören tillhör då den reserverade tenanten `operator`, som har **Tenants**-konsolen och äger licensen. (För fler operatörer: `lumen users add --tenant operator --group admin NAMN`.)
2. Installera en **Operator-licens** under *Settings, Licence*. Utan den är konsolen stängd, Lumen räknar bara vad varje tenant skickar, och inget upprätthålls: en utgången licens stänger aldrig ute dina kunder.
3. Öppna **Tenants**. Tenants som redan fanns finns där, med standardinställningar (stödåtkomst: *när som helst*, eftersom de fanns före inställningen). Lägg till nya med **+ Add tenant**.

## Till vardags

| Jag vill... | Gör så här |
|---|---|
| Lägga till en kund | Tenants, + Add tenant. Ange ett id (gemener, används i filnamn, kan inte ändras), ett namn och den första administratören. Ett lösenord skapas och visas en gång. |
| Sätta gränser | Öppna tenanten, fyll i gränserna, kryssa **Refuse** för att upprätthålla dem, Spara. Utan Refuse varnar de bara, på din sida och på kundens Settings-sida. |
| Stoppa en kund som inte betalat | Öppna tenanten, Suspend, ange en orsak. Välj i tenantens detaljer om inkommande data *avvisas* (agenterna försöker igen) eller *kastas* (agenterna tror att allt är bra). Resume återställer allt direkt. |
| Hjälpa en kund | Be dem tillåta supportåtkomst (Settings, Support access, Allow access). Öppna sedan tenanten och **Go into tenant**: skrivskyddat om du inte kryssar för ändringar, 30 minuter till 4 timmar. En banner visar att du är inne, och **Leave tenant** avslutar. |
| Fakturera | Tenants, Usage: per tenant och dag, eller **Download CSV** (för vald period och tenant). |
| Ge en kund deras data | Öppna tenanten, **Export** (en zip med inställningar och dokument utan hemligheter). Telemetrin finns i de dagliga backuperna. |
| Ta bort en kund | Öppna tenanten, **Remove...**, skriv dess id. Allt som tillhör tenanten raderas, också arkiv och dagliga backuper, och Lumen kontrollerar att inget finns kvar. Användningsposter och åtkomstloggen behålls. |

## Vad kunden ser

Under **Settings**: sin användning mot sina gränser (och en varning nära en gräns), **Support access** (av, bara när de tillåter det, när som helst) och en **Access log** som listar varje gång en operatör varit inne och allt som ändrats.

## Bra att veta

- **Gränserna** gäller kundens egna användare och nycklar, inte en operatör inne i tenanten, som alltid kan rätta till saker.
- **Hostar** räknas som de som skickat mätvärden de senaste 24 timmarna. En host som redan rapporterar nekas aldrig av hostgränsen.
- **En borttagning som misslyckas** visar steget och orsaken på tenantens sida och kan köras om. Stoppa tenantens agenter först om data fortsätter komma.
- **Konfigurationsdumpar** i backupkatalogen innehåller varje tenants inställningar tills de ersätts (de senaste 30 sparas). Borttagningen säger det.
- **En server utvärderar allt.** Det finns inget läge för hög tillgänglighet än.
