# Lumens utgåvor: stadgan

Svenska · [English](EDITIONS.md)

> **Status: utkast 0.1.** Det här är en avsiktsförklaring. Det är inget avtal och inte juridisk rådgivning. Punkter inom [hakparenteser] är ännu inte beslutade. Låt en jurist granska texten innan den publiceras som ett löfte.

## 1. Varför det här dokumentet finns

Lumen utvecklas öppet, och en del av det kommer att säljas. Den som bygger på Lumen behöver veta vad som förblir gratis, vad som kostar pengar och varför, och att gränsen inte flyttas till deras nackdel senare. Den här stadgan fastställer gränsen offentligt.

## 2. Våra löften

1. **Community är en riktig produkt.** Allt som en ensam administratör eller ett litet team behöver för att övervaka sin egen infrastruktur finns i Community-utgåvan: insamling, sökning, dashboards, agenter, larm och backup. Det finns inga konstgjorda gränser för antal hostar, användare, dashboards, datamängd eller lagringstid.
2. **Inga tillbakadragningar.** En funktion som har släppts i en Community-version flyttas aldrig till en betald utgåva. En funktion får flyttas åt andra hållet, från en betald utgåva till Community, när som helst.
3. **Säkerhet är aldrig en betald funktion.** Isolering mellan tenants, autentisering, behörighetskontroller, kryptering av lagrade hemligheter och säkerhetsrättningar finns i alla utgåvor. Säkerhetsrättningar släpps till alla utgåvor samtidigt.
4. **Din data är din.** Data kan alltid exporteras via API:t och backupfilerna, i enkla dokumenterade format som går att läsa utan Lumen.
5. **Ingen telefon hem.** Ingen av utgåvorna skickar användningsdata någonstans. Licenskontroll sker offline.
6. **Ingen utelåsning.** Om en Enterprise-licens går ut raderas ingenting och ingen låses ute från sin data (se avsnitt 6).
7. **Agenter är gratis.** Agenterna för Linux, Windows och Docker, och allt de samlar in, är Community-funktioner.
8. **Ändringar sker öppet.** Den här stadgan ändras bara öppet (avsnitt 8).

## 3. Utgåvorna i överblick

*Finns* betyder att funktionen finns i koden i dag. *Planerad* betyder att den ligger på färdplanen och kan ändras innan den släpps.

| Förmåga | Community | Enterprise | Operator-tillägg |
|---|---|---|---|
| OTLP-insamling, sökning, dashboards, mätvärdesutforskare, traces, loggar | Ja (finns) | Ja | Ja |
| Agenter, hostprestanda, tjänster och containrar, Hosts och Instances, Nextcloud-övervakning, upp/ner-status | Ja (finns) | Ja | Ja |
| Lokal backup och arkiv, lagringstid | Ja (finns) | Ja | Ja |
| Användare, grupper, områdesbehörigheter, API-nycklar | Ja (finns) | Ja | Ja |
| Isolering mellan tenants | Ja (finns) | Ja | Ja |
| Sidnamn och logga | För hela installationen (finns) | För hela installationen | Per tenant (planerad) |
| Larmregler på mätvärden, status, loggar och traces; e-post, webhook, Slack och Teams; kvittera; enkla tystningar; startregler | Ja (finns, utom trace-regler) | Ja | Ja |
| OIDC-inloggning med en leverantör | Ja (planerad) | Ja | Ja |
| Granskningslogg: vem ändrade vad, synlig för administratörer | Ja (planerad) | Ja | Ja |
| SAML, SCIM, LDAP-gruppmappning; påtvingad MFA; policyer för API-nycklar och sessioner; maskning av personuppgifter vid insamling | | Ja (planerad) | |
| Manipuleringsskyddad granskningslogg med lång lagringstid och SIEM-export | | Ja (planerad) | |
| Jira, ServiceNow, PagerDuty, Opsgenie; eskaleringspolicyer och jourscheman; larmdirigering, gruppering och mallar; återkommande underhållsfönster med godkännande | | Ja (Jira, ServiceNow, PagerDuty och Opsgenie finns, resten är planerat) | |
| Hög tillgänglighet, ClickHouse-kluster, lagringsnivåer och nedsampling, uppgradering utan driftstopp, återställning till en tidpunkt | | Ja (planerad) | |
| Krypterade backuper utanför servern (till exempel S3), juridisk spärr | | Ja (planerad) | |
| Kubernetes-operator, Terraform-provider | | Ja (planerad) | |
| SLO:er och burn-rate-larm, SLA-rapporter, statussidor, uppkopplingskontroller från flera platser, prognoser | | Ja (planerad) | |
| Tenant-konsol, kvoter, användningsmätning och export | | | Ja (planerad) |
| Branding, domäner och lagringstid per tenant | | | Ja (planerad) |
| Kundvända rapporter, supportåtkomst med medgivande, dedikerad lagring per tenant | | | Ja (planerad) |
| Support med SLA, versioner med långtidsstöd, signerade byggen | Community-support via ärenden | Ja | Ja |

**Operator-tillägget** är till för att driva Lumen *åt andra*: en hostingleverantör med kunder, eller ett företag med interna verksamheter som var och en behöver isolerad data, gränser och rapporter. Det kan kombineras med Enterprise.

## 4. Hur vi bestämmer var en funktion hamnar

Vi ställer de här frågorna i ordning och stannar vid första svaret:

1. Behövs den för datans säkerhet, eller för att produkten alls ska fungera? **Community.**
2. Skulle en administratör eller ett litet team på en server använda den? **Community.**
3. Är den viktig främst för styrning och regelefterlevnad, för skala över team och platser, för integration med företagets system, eller för att driva Lumen som tjänst åt kunder? **Enterprise** eller **Operator.**
4. Är det ett gränsfall? **Community.**

## 5. Det som aldrig kräver licens

Agenter; API:t och dataexport; isolering mellan tenants och varje behörighetskontroll; kryptering av lagrade hemligheter; grundläggande användare, grupper och behörigheter; larmregler med notiser via e-post, webhook, Slack och Teams; lokal backup och arkiv; säkerhetsrättningar; dokumentation.

## 6. Hur licensen fungerar

- En Enterprise- eller Operator-licens är en **signerad fil som verifieras offline**. Den anger organisationen, utgåvorna, utgångsdatum och, för Operator-tillägget, eventuella gränser.
- Gränser i en licens är **mjuka**: Lumen visar varningar vid 90 procent och när en gräns överskrids. Lumen vägrar inte ta emot data på grund av en licens. (Kvoter som en operatör sätter för sina egna kunder är något annat; de är operatörens egen konfiguration.)
- **När en licens går ut** gäller en frist på 30 dagar med en banner. Därefter slutar Enterprise-funktionerna att fungera. Community-funktionerna och all data påverkas inte, och ingenting raderas. Ett lokalt administratörskonto fungerar alltid, även om single sign-on inte längre är tillgängligt.

## 7. Bidrag och licenser

- Kärnan är licensierad under [Apache-2.0 — ska bekräftas]. Katalogen `ee/` har en egen kommersiell licens.
- Bidrag till kärnan tas emot under [DCO eller CLA — avgörs innan första externa bidraget accepteras].
- Enterprise-kod skrivs av underhållarna eller på uppdrag.
- Namnet och loggan skyddas av en varumärkespolicy [ska skrivas].

## 8. Hur stadgan ändras

Ändringar föreslås i ett offentligt ärende eller en pull request och meddelas i ändringsloggen. Löftena i avsnitt 2 får förstärkas, aldrig försvagas. Tabellen i avsnitt 3 får ändras på två sätt: en funktion får flyttas från Enterprise eller Operator till Community när som helst, och en *ny* funktion får placeras i vilken utgåva som helst. En släppt Community-funktion flyttas aldrig.

## 9. Öppna punkter innan detta publiceras

- [ ] Juridisk granskning av löftena och licensvillkoren
- [ ] Besluta kärnans licens
- [ ] Välja mellan DCO och CLA
- [ ] Varumärkespolicy för namn och logga
- [ ] Slutgiltiga utgåvonamn
- [ ] Kontaktadress för licensfrågor
- [ ] Avgöra om OIDC med en leverantör hör hemma i Community (stadgan förutsätter att det gör det)
