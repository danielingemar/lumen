# Licenser

Svenska · [English](LICENSING.md)

Så fungerar Enterprise- och Operator-licenser, för den **utgivare** som utfärdar dem och den **kund** som använder dem. Löftena bakom finns i [EDITIONS.sv.md](EDITIONS.sv.md): kontrollen sker offline, Lumen kontaktar aldrig någon, ingenting raderas när en licens går ut och Community-funktionerna fungerar alltid.

## Så fungerar det på en minut

- En licens är en liten fil som är signerad med utgivarens **privata** nyckel. Programmet innehåller den matchande **publika** nyckeln och kan därför kontrollera signaturen utan något nätverk.
- Filen anger vem den gäller, vilka utgåvor den omfattar, när den går ut och, om man vill, mjuka gränser för hostar och tenants.
- Utan licens är Lumen **Community-utgåvan**. Med en giltig licens fungerar Enterprise-funktionerna (i dag: notiser till Jira, ServiceNow, PagerDuty och Opsgenie).
- 30 dagar före slutet visas en gul påminnelse för administratörer. Efter slutdatumet gäller **30 dagars frist** då allt fortsätter att fungera, med en röd påminnelse. Därefter slutar Enterprise-funktionerna. Community-funktionerna, all data och alla inställningar finns kvar som de är, och en förnyad licens slår på allt igen direkt.

## För utgivaren

### Engångsinställning

```bash
go run ./cmd/lumen-license keygen --id main --dir keys
```

Det skriver `keys/main.key` (**privat, hemlig**) och `keys/main.pub` (publik). Därefter:

```bash
cp keys/main.pub internal/license/keys/main.pub
git add internal/license/keys/main.pub && git commit -m "Add the licence verification key"
```

Bara `.pub`-filen checkas in. `*.key`, `*.license`, `/keys/` och `issued.jsonl` står i `.gitignore` så att de inte kan checkas in av misstag. **Håll den privata nyckeln offline** (ett krypterat USB-minne eller en hårdvarunyckel räcker till att börja med), spara en andra krypterad kopia på ett annat ställe, och lägg den aldrig på en server som kunder når eller i ett CI-system. Den som har den kan utfärda licenser.

### Bygga Enterprise-utgåvan

Enterprise-koden kompileras in bara med byggtaggen `enterprise`, och de publika nycklarna kompileras in från `internal/license/keys/`.

```bash
# Docker: sätt det en gång i deploy/.env och bygg som vanligt
echo "LUMEN_BUILD_TAGS=enterprise" >> deploy/.env
sudo docker compose -f deploy/docker-compose.yml up -d --build

# eller utan Docker
go build -tags enterprise -o lumen ./cmd/lumen
```

Ett bygge utan din publika nyckel litar inte på någon licens, så det är ett Community-bygge oavsett vilken fil det får.

### Utfärda en licens

```bash
go run ./cmd/lumen-license issue --key keys/main.key --customer "ACME AB" \
  --editions enterprise --days 365 --hosts 50 --ledger issued.jsonl --out acme.license
```

- `--editions` är `enterprise`, `operator` eller båda (`enterprise,operator`).
- Ange slutet med `--days 365` eller `--expires 2027-12-31` (slutet av den dagen, UTC).
- `--hosts` och `--tenants` är **mjuka** gränser: Lumen varnar vid 90 procent och när de överskrids, och nekar aldrig data.
- `--issued 2026-12-31` datumsätter licensen tidigare eller senare än i dag, till exempel så att en förnyelse börjar när den gamla slutar.
- `--ledger issued.jsonl` lägger till en rad per licens: din egen förteckning över vad du utfärdat, till vem och till när. Verktyget ringer ingenstans; filen är din att spara (och säkerhetskopiera, men inte i det publika repot).
- Skicka `.license`-filen till kunden. Den är inte hemlig, men den är gjord för dem.
- Kontrollera en fil när som helst med `go run ./cmd/lumen-license inspect acme.license --pub keys/main.pub`.

En provlicens är en licens med kort slutdatum (`--days 30`). En förnyelse är en ny licens: kunden installerar den över den gamla.

### Förnya, återkalla, byta nycklar

- **Förnya:** utfärda en ny licens och skicka den. Kunden installerar den under Inställningar (eller byter filen, se nedan).
- **Återkalla:** eftersom inget kontrolleras online går en licens inte att kalla tillbaka. Det är medvetet. Använd **korta perioder** (ett år, eller kortare för kunder du är osäker på) och förnya helt enkelt inte.
- **Byta nycklar:** du kan ha flera `.pub`-filer, och en licens anger vilken nyckel som signerade den. För att avveckla en nyckel utfärdar du nya licenser med en ny nyckel, släpper en version som innehåller båda publika nycklarna tills alla gamla licenser gått ut, och tar sedan bort den gamla. Om den privata nyckeln någonsin hamnat hos någon annan gör du exakt så direkt.

## För kunden

- **Installera:** logga in som administratör i den tenant som äger installationen, öppna **Settings** och välj under **Licence** filen `.license` (eller klistra in innehållet) och tryck **Install licence**. En felaktig eller ändrad fil nekas med orsak, och inget ändras.
- **Som fil:** sätt `LUMEN_LICENSE_FILE` till en filsökväg. Den läses vid start och sedan varje minut, så en förnyelse kan läggas dit som fil. En licens som kommer från en fil förnyas genom att filen byts; gränssnittet säger det.
- **Vem som hanterar den:** licensen tillhör installationen, så i en installation med flera tenants får bara den tenant som äger den (den som skapades vid installationen, `LUMEN_ADMIN_TENANT`) se eller ändra den. En annan kunds administratör kan inte ta bort den.

| Tillstånd | Betydelse | Enterprise-funktioner |
|---|---|---|
| none | Ingen licens: Community-utgåvan | av |
| valid | Giltig, mer än 30 dagar kvar | på |
| expiring | Giltig, 30 dagar eller mindre kvar (gul påminnelse) | på |
| grace | Slutade för mindre än 30 dagar sedan (röd påminnelse) | **fortfarande på** |
| expired | Slutade för mer än 30 dagar sedan (röd påminnelse) | av |
| cannot be used | En sparad licens som inte längre verifierar (till exempel att bygget ändrats så att det litar på andra nycklar). Orsaken visas | av |

När Enterprise-funktioner är av **raderas ingenting**: kanaler och inställningar finns kvar, märks "licence needed" och fungerar igen i samma stund som en giltig licens installeras. En notiskanal som var utestängd får veta vilka larm som brinner så fort den får användas igen.

### Meddelanden du kan se

| Meddelande | Betydelse |
|---|---|
| the signature does not match | Filen ändrades efter att den gjordes (även ett enda tecken), eller gjordes för något annat |
| signed with the key "x", which this build does not trust | En annan utgivares licens, eller ett bygge utan din publika nyckel |
| this build of Lumen trusts no licence keys | Ett Community-bygge av programmet: använd bygget du fick tillsammans med licensen |
| that is not a Lumen licence file | Inte `.license`-filen (försök klistra in hela filen, med klammerparenteser) |
| the licence in force is read from the file … | `LUMEN_LICENSE_FILE` är satt: byt den filen i stället |

## Ärliga begränsningar

- **Källkoden är öppen.** Den som kan bygga Lumen kan ändra kontrollen. Licenskontrollen håller ärliga kunder ärliga och gör oavsiktlig användning lätt att se. Det är avtalet som skyddar den kommersiella utgåvan. Har viss Enterprise-kod verkligt värde, överväg att ha den i ett privat repo (se resonemanget om utgåvor).
- **En klocka som ställts tillbaka** förlänger en licens. Lumen försöker inte försvara sig mot det (det skulle kräva en onlinekontroll, vilket vi lovat att aldrig göra).
- **Hostantalet** i varningarna är den inloggade tenantens tills Operator-tillägget har tenant-poster som kan räkna hela installationen.
