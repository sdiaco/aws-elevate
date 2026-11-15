# aws-elevate: design v2

| | |
|---|---|
| **Stato** | Draft, fase di design (nessuna implementazione). Sostituisce la v1 dove indicato |
| **Data** | 2026-11-14, aggiornato il 2026-11-15 |
| **Ambito** | Accesso elevato temporaneo (JIT) per AWS IAM Identity Center |
| **Design precedente** | v1 del 2026-11-08, documento interno non pubblicato. I rimandi "v1 §…" si riferiscono a quello |
| **Nome** | Progetto e comando: `aws-elevate` (prima `sso-elevate`). Pacchetto: `aws-elevate-cli`. Nel testo, "elevate" per brevità. Le risorse AWS tengono il prefisso `elevate` (stack `elevate`, `ElevateAdmin`, eventi `elevate.*`) |
| **Evidenze** | Spike fasi A, B1, B2 e C, eseguiti in un'organizzazione AWS di test. Codice e report sono documentazione interna, non pubblicata |

Come leggere questo documento: le sezioni 1–13 contengono solo scelte discusse e approvate. Non ci sono proposte in sospeso (§14). La §15 elenca le ipotesi dello spike C; gli esiti sono nel report interno della fase C e le conseguenze sono già riportate nelle sezioni interessate.

---

## 1. Obiettivo

Gli utenti accedono ad AWS con IAM Identity Center. elevate concede permessi elevati **temporanei** su richiesta, da CLI o da una UI locale, con scadenza imposta da AWS e non dal client.

Rispetto ad AWS TEAM deve essere:

1. **più leggero**: uno stack serverless, nessuna web app ospitata, nessun componente nel management account;
2. **più rigoroso sulla scadenza**: le credenziali già emesse smettono di funzionare all'orario esatto;
3. **quasi plug and play**: dall'installazione al primo grant in circa mezz'ora, senza repo da clonare né pipeline da configurare.

### 1.1 Cosa resta della v1 e cosa cambia

Il nucleo della v1 è confermato: nessuna alternativa trovata (TEAM, sso-elevator, prodotti commerciali, account access manager) ottiene la scadenza reale con un'impronta minore. Cambia la periferia.

| Tema | v1 | v2 |
|---|---|---|
| Meccanismo di accesso | PS effimero per grant, scadenza nella policy, scoping per tag `jit` | **Invariato** |
| Orchestrazione | Step Functions Standard, una execution per grant | **Invariata** |
| Ingresso | API Gateway REST con auth IAM | **Invariato** |
| Definizione dei permessi | Template YAML con policy e parametri, in un repo Git | **Permission set esistenti** in Identity Center. elevate non scrive mai policy proprie |
| Regole | File YAML in PR, compilati dalla CI | Modulo nella UI o file da CLI, pubblicati direttamente su AppConfig |
| Revisione della config | PR obbligatoria | **Traccia** di chi ha pubblicato cosa (CloudTrail). Nessuna approvazione |
| Stack | 2 (`elevate` + `elevate-guardrails` obbligatorio con canary) | 1. SCP come stack **opzionale**, senza canary |
| Lambda | 10 | 2 (`api`, `engine`) più il modulo Slack opzionale |
| Ruoli del servizio | builder / activator / revoker separati | Un solo ruolo `engine` |
| Rilevamento drift | validator su CloudTrail + reconciler ogni 15 min | **Nessuno**. Nessun cron. Resta la traccia CloudTrail |
| Revoca anticipata | delete (~17 s) | riscrittura della scadenza + delete (~9 s attesi) |
| Avvisi | Slack | SNS (email e altro). Slack opzionale |
| Parametri nei permessi | sì | **Fase 2** |
| Durata massima | 1h | default 1h, alzabile per regola fino a 12h |
| UI | locale, dopo il motore | locale, parte del prodotto: richieste, revoca, regole, storico, aggiornamenti |

## 2. Goals, non-goals, anti-goals

### Goals
- **G1.** Elenco chiuso: si può richiedere solo ciò che una regola prevede (permission set, account, durata massima). Tutto il resto è negato.
- **G2.** Durata limitata, scelta dal richiedente entro il massimo della regola. La fine la impone AWS.
- **G3.** Approvazione automatica o manuale per regola. Chi approva è diverso da chi richiede.
- **G4.** CLI dentro la AWS CLI (`aws elevate …`) e UI web locale (`aws elevate ui`), entrambe con le credenziali SSO già presenti.
- **G5.** Scadenza effettiva: dopo la fine del grant nessuna credenziale emessa per quel grant può compiere azioni.
- **G6.** Il servizio può scrivere solo permission set marcati `jit`. Quelli degli admin restano irraggiungibili.
- **G7.** Ogni passaggio è tracciato (history della execution, storico, CloudTrail) ed emesso come evento consumabile.
- **G8.** Installazione e aggiornamento semplici: un comando di installazione guidata (o un "Launch Stack"), un comando per il client, una procedura guidata per la prima regola.

### Non-goals
- UI web ospitata.
- Accesso al management account (escluso per costruzione, §4.1).
- Break-glass: escluso per decisione esplicita.
- Windows.
- Pubblicazione della config da CI come funzione del prodotto. Restano export e import da CLI.
- Rilevare o correggere modifiche manuali fatte dagli admin di Identity Center ai PS `jit`.

### Anti-goals
- Credenziali utilizzabili oltre la scadenza del grant.
- Un utente che ottiene un accesso non previsto da una regola.
- Una modifica alle regole di cui non resta traccia dell'autore.
- Uno sviluppatore che deve clonare repo o configurare a mano ruoli e profili.
- Residui negli account target che richiedono pulizia manuale.
- Cron di pulizia, o garanzie che dipendono da un cron.

## 3. Concetti

| Concetto | Definizione |
|---|---|
| **Permission set (PS)** | Modello di permessi di Identity Center. Lo crea e lo modifica l'admin in AWS (console o IaC), mai da elevate |
| **Regola** | "Il gruppo G può chiedere il PS P sugli account A, per una durata fino a D, con approvazione automatica o manuale". È l'unica cosa che si configura in elevate |
| **Grant** | Una richiesta e il suo ciclo di vita, dalla creazione alla chiusura |
| **PS effimero** | `JIT-<grantId>`: copia del PS della regola più la regola di scadenza, assegnata a un solo utente su un solo account e cancellata a fine grant |

### 3.1 Regola

| Campo | Contenuto |
|---|---|
| Chi può chiedere | Un gruppo di Identity Center |
| Permission set | Uno, scelto dall'elenco di quelli esistenti (nome e ARN) |
| Dove | Uno o più account dell'organizzazione, scelti dall'elenco letto da Organizations. Il management account non compare |
| Durata | Proposta e massima. Uguali per tutti quelli che possono usare la regola. Massimo assoluto 12h |
| Approvazione | Automatica oppure manuale |
| Gruppo che approva | Un gruppo di Identity Center, solo se manuale |
| Privileged | Casella decisa dall'admin. Obbliga all'approvazione manuale ed evidenzia la regola e i suoi grant ovunque |

Più persone possono avere lo stesso permesso con durate diverse nello stesso momento: ogni grant ha il suo PS effimero con la sua scadenza.

### 3.2 Stati di un grant

Un solo vocabolario per UI, CLI, eventi e database.

| Stato | Etichetta | Significato |
|---|---|---|
| `pending` | In attesa | Richiesta creata, serve un approvatore |
| `activating` | In attivazione | Approvata o automatica; il servizio sta creando l'accesso |
| `active` | Attivo | Accesso utilizzabile |
| `revoking` | In revoca | Revoca in corso |
| `revoke_failed` | Revoca fallita | Serve un intervento; compare in Problemi |
| `closed` | Chiuso | Finito, con un motivo |

| Motivo di chiusura | Etichetta | Accesso mai dato |
|---|---|---|
| `expired` | Scaduto | no |
| `revoked` | Revocato (da chi, perché) | no |
| `rejected` | Rifiutato | sì |
| `timed_out` | Nessuna risposta in tempo | sì |
| `cancelled` | Annullato dal richiedente | sì |
| `invalidated` | Regola cambiata mentre era in attesa | sì |
| `failed` | Attivazione fallita | sì |

Etichette che non sono stati: **Privileged**, **Automatico / Manuale**.

### 3.3 Ruoli

| Ruolo | Da dove viene | Può |
|---|---|---|
| Richiedente | Gruppo "chi può chiedere" di una regola | Chiedere, annullare, revocare i propri grant |
| Approvatore | Gruppo che approva di una regola | Approvare o rifiutare richieste altrui su quella regola, revocarne i grant |
| Admin | PS `ElevateAdmin` | Tutto, più regole e revoca di qualsiasi grant |
| Auditor | Gruppo indicato in un parametro facoltativo dello stack. Se è vuoto il ruolo non esiste | Leggere tutto, esportare |

## 4. Architettura

### 4.1 Account

| Account | Contenuto |
|---|---|
| **Service account** = delegated admin di Identity Center | Tutto il servizio |
| **Management account** | Niente. Serve una volta sola per registrare il delegated admin, se manca. Ospita lo stack SCP solo se si sceglie di installarlo (§8.4) |
| **Account target** | Nessun componente. Ricevono i ruoli `AWSReservedSSO_JIT-*` creati da Identity Center. Devono già contenere le eventuali policy customer managed richiamate dai PS |

Region del servizio = home region di Identity Center.

Il delegated admin è l'unico prerequisito organizzativo, e si spiega come "non lavorare da root". Chi non l'ha lo registra una volta dal management account:

```bash
aws organizations register-delegated-administrator \
  --account-id <id dell'account scelto> \
  --service-principal sso.amazonaws.com
```

Dal delegated admin elevate assegna accessi su tutti gli account membri; il management account resta escluso da AWS stessa (spike S15).

### 4.2 Diagramma

```
 aws elevate <comando>          aws elevate ui (server locale su 127.0.0.1)
        │                                │
        └────── SigV4, credenziali SSO (PS ElevateRequester / ElevateAdmin) ──────┐
                                                                                  ▼
                                        API Gateway REST (auth AWS_IAM, resource policy)
                                                                                  │
                                                                                  ▼
                 AppConfig (regole) ◄── legge ── Lambda api ──► DynamoDB (grant, storico)
                        ▲                            │ StartExecution / SendTask*
   admin pubblica       │                            ▼
   direttamente ────────┘         ┌──── Step Functions (1 execution = 1 grant) ─────┐
   (PS ElevateAdmin)              │ [manuale] attesa approvazione (task token)      │
                                  │   → attivazione (engine)                        │
                                  │   → attivo (task token, timeout = durata)       │
                                  │   → revoca (engine)                             │
                                  │ Catch su ogni passo → revoca                    │
                                  └───────┬─────────────────────────┬──────────────┘
                                          │ invoca                  │ PutEvents (integrazione nativa)
                                          ▼                         ▼
                                   Lambda engine ──► Identity Center      EventBridge bus elevate-events
                                   (solo PS jit)                             ├─► regola "critici" ─► SNS ─► email
                                                                             └─► consumer liberi (Slack opzionale)
```

### 4.3 Componenti

| Componente | Responsabilità |
|---|---|
| **API Gateway** | Unica porta d'ingresso. Auth `AWS_IAM`; la resource policy ammette solo i ruoli SSO `ElevateRequester` ed `ElevateAdmin` del service account (§8.1) |
| **Lambda `api`** | Riceve input esterno. Ricava l'identità dalla firma, risolve utente e gruppi, valida contro le regole, avvia o fa avanzare le execution, risponde alle viste. **Nessuna scrittura su Identity Center** |
| **Step Functions** | Ciclo di vita del grant: timer, retry, catch, history. Emette gli eventi |
| **Lambda `engine`** | Unico componente che scrive su Identity Center, e solo su PS `jit`. **Non riceve input esterno**: lavora sullo stato dell'execution e sul record del grant. Attiva e revoca |
| **DynamoDB `grants`** | Stato dei grant, timeline, storico per 13 mesi |
| **AppConfig** | Le regole. Versioni numerate e immutabili |
| **EventBridge bus `elevate-events`** | Contratto pubblico degli eventi (§9) |
| **SNS `elevate-alerts`** | Avvisi critici via email o altri iscritti |
| **SNS `elevate-approvals`** | Richieste manuali in attesa, per chi si iscrive (§9.2) |
| **PS di servizio** | `ElevateRequester` e `ElevateAdmin`, creati dallo stack e assegnati ai gruppi indicati come parametri |
| **Modulo Slack** (opzionale) | Notifiche e bottoni Approva / Rifiuta. Senza, tutto funziona da CLI e UI |

Il confine di sicurezza che resta dalla v1: chi legge input non fidato (`api`) non può toccare Identity Center; chi può toccarlo (`engine`) non legge input esterno.

## 5. Modello di accesso: permission set effimero

### 5.1 Contenuto

All'attivazione l'engine legge il PS della regola **in quel momento** e crea:

| Proprietà | Valore |
|---|---|
| Nome | `JIT-<grantId>`, con `grantId` di 10 caratteri base32 minuscoli |
| Policy AWS managed | Le stesse del PS di origine |
| Policy customer managed | Gli stessi riferimenti del PS di origine. Devono esistere nell'account target |
| Permissions boundary | Lo stesso del PS di origine, se presente |
| Inline policy | Quella del PS di origine **più lo statement di scadenza** |
| Durata di sessione | Pari alla durata del grant, arrotondata all'ora superiore (minimo 1h, massimo 12h). In console si resta dentro per tutto il grant; la fine la impone comunque lo statement di scadenza |
| Description | Motivo della richiesta, sanificato e troncato |
| Tag | §5.3 |

Statement di scadenza, sempre presente:

```json
{
  "Sid": "JitExpiry",
  "Effect": "Deny",
  "Action": "*",
  "Resource": "*",
  "Condition": { "DateGreaterThan": { "aws:CurrentTime": "<fine grant, ISO-8601 UTC>" } }
}
```

Poi assegna il PS effimero al solo richiedente sul solo account scelto. Il PS di origine non viene mai modificato né assegnato.

Conseguenza voluta: se un admin cambia il PS di origine in AWS, i grant successivi ricevono il contenuto nuovo, anche su regole ad approvazione automatica. È responsabilità dell'admin, e la modifica resta in CloudTrail. I grant già attivi non cambiano.

### 5.2 Scadenza e durata
- `fine grant = inizio dell'attivazione + durata richiesta`. Il tempo di attivazione è compreso nella durata.
- Durata: proposta dalla regola, modificabile dal richiedente fino al massimo della regola. Massimo assoluto 12h.

### 5.3 Tag del PS effimero

| Tag | Valore |
|---|---|
| `jit` | `true`, impostato solo alla creazione |
| `jit:grantId` | id del grant |
| `jit:requester` | username del richiedente |
| `jit:approver` | username dell'approvatore, oppure `auto` |
| `jit:rule` | id della regola |
| `jit:source` | nome del PS di origine |
| `jit:account` | account target |
| `jit:configVersion` | numero di versione AppConfig |
| `jit:privileged` | `true` / `false` |

### 5.4 Limiti verificati al salvataggio di una regola

Li controllano UI e CLI, perché dipendono dal contenuto del PS e non possono stare nello schema della config (§7.1).

- Inline finale (origine + scadenza, circa 150 caratteri) entro 10.240 caratteri esclusi gli spazi, e 32.768 byte in totale: se sfora, il salvataggio è bloccato con il messaggio di quanto. Sono i limiti documentati da AWS per un permission set, non aumentabili.
- Massimo 10 policy managed per ruolo (quota IAM, aumentabile dall'organizzazione): stesso blocco.

Il PS di origine può cambiare dopo il salvataggio. Se al momento del grant uno dei due limiti è superato, o una policy customer managed manca nell'account target, l'attivazione fallisce: stato `closed / failed`, evento, riga in Problemi con il motivo preciso.

### 5.5 Cosa la scadenza non copre

Da scrivere nella documentazione per gli admin:

- **Ruoli assunti e credenziali create.** Se il PS permette `sts:AssumeRole`, o di creare access key o ruoli IAM, ciò che viene assunto o creato sopravvive alla scadenza del grant. elevate non lo controlla: l'admin marca la regola come privileged.
- **Operazioni lunghe.** Un'operazione avviata con il ruolo JIT che continua a usare i permessi dell'utente si interrompe alla scadenza. Per questo la durata è richiedibile fino al massimo della regola.

## 6. Flussi

### 6.1 Richiesta
1. `aws elevate request` oppure vista Richiedi: regola, account, durata, motivo (obbligatorio).
2. `api` ricava l'identità dalla firma (§8.1), legge le regole correnti e registra il numero di versione, verifica che l'utente sia nel gruppo della regola, valida account e durata, controlla unicità (un solo grant in corso per utente, PS e account) e rate limit.
3. `api` scrive il grant e avvia l'execution con **nome = `grantId`**. Due protezioni contro i doppioni: il controllo di unicità ferma una seconda richiesta uguale dello stesso utente; il nome dell'execution impedisce che un nuovo tentativo di `api` avvii due volte lo stesso grant.
4. Approvazione automatica → attivazione. Manuale → stato `pending`.

### 6.2 Approvazione
- `aws elevate approve <id>` (o `--reject`), vista In attesa, oppure bottone Slack se il modulo è installato.
- `api` verifica: l'approvatore è nel gruppo che approva della regola; è diverso dal richiedente; il grant è `pending`; la regola esiste ancora nella config corrente e la richiesta è ancora conforme. Se non lo è → `closed / invalidated`.
- Il richiedente può annullare finché è `pending` → `closed / cancelled`.
- Nessuna risposta entro il tempo di attesa (30 minuti) → `closed / timed_out`.

### 6.3 Attivazione
L'engine, a partire dal record del grant:
1. rilegge la regola nella versione registrata e verifica che il grant sia coerente con essa;
2. legge il PS di origine e crea il PS effimero (§5.1);
3. crea l'assegnazione e attende `SUCCEEDED`.

Stato `active`. Il client in attesa scrive il profilo (§10.3). Latenza attesa circa 11 s (spike S3, misurata sul modello v1).

**Pronto all'uso.** Dopo `SUCCEEDED` il portale di accesso rifiuta le credenziali ancora per 6–7 s, e la prima chiamata può essere negata per altri 1–2 s (spike C12; gli 11 s sopra li comprendono già). Il servizio non può verificarlo, perché serve il token SSO dell'utente. Lo fa il client: CLI e UI riprovano a ottenere le credenziali del grant finché una chiamata riesce, e solo allora mostrano "pronto", scrivono il profilo e abilitano "Apri in console".

### 6.4 Scadenza e revoca
- **Scadenza:** timeout dello stato "attivo" → revoca. L'accesso è già negato dalla policy (spike S7: negato 0,5 s dopo la scadenza); la revoca è solo pulizia.
- **Revoca anticipata:** dal richiedente, da un approvatore della regola o da un admin, con motivo.
- **Revoca mentre il grant è in attivazione:** rifiutata subito con "in attivazione, riprova tra qualche secondo". Nessuna coda e nessuna richiesta sospesa. La UI tiene il pulsante Revoca disattivato finché il grant non è attivo.
- **Passi dell'engine**, idempotenti (ognuno tollera "già fatto"):
  1. riscrive la inline con scadenza = adesso, così le sessioni aperte vengono negate subito;
  2. cancella l'assegnazione e attende `SUCCEEDED`;
  3. cancella il PS effimero;
  4. aggiorna il grant a `closed`.

### 6.5 Errori
- Retry con attese crescenti su throttling e `ConflictException`.
- `Catch` su ogni passo → revoca.
- **Revoca che continua a fallire:** stato `revoke_failed`, evento `elevate.grant.error`, avviso su SNS, riga in Problemi. Chi ha chiesto la revoca vede che non è riuscita, mai un falso "revocato". Si chiude riprendendo l'execution dal punto di errore (redrive), da UI o CLI.
- **Forma della state machine per il redrive (spike C8):** il `Catch` della revoca porta all'evento di errore e poi a un secondo task di revoca senza `Catch`, su cui l'execution fallisce. Il redrive riparte da quel task, ritenta la revoca e chiude il grant, senza rieseguire l'attivazione. Un `Catch` che finisce su uno stato `Fail` non va usato: il redrive viene accettato ma non ritenta nulla.

| Caso | Accesso dopo il fallimento |
|---|---|
| Scadenza naturale, pulizia fallita | Già negato dalla policy. Restano un PS e un'assegnazione inerti |
| Revoca anticipata fallita | Vivo fino alla scadenza originale. La UI mostra per quanti minuti |

### 6.6 Modifica delle regole e grant in corso

Nessun cron, quindi:

- **Richieste nuove:** seguono le regole nuove entro circa 30 secondi (`api` tiene le regole in memoria e le rinfresca a intervalli, §7.1).
- **Richieste in attesa:** verificate di nuovo all'approvazione; se non più conformi, `invalidated`.
- **Grant attivi:** restano validi fino alla loro scadenza. Sono stati concessi quando la regola lo permetteva.

Per chiuderli prima basta la revoca normale: la vista Attivi con selezione multipla, oppure `aws elevate revoke <id> <id> …`. Non esiste un concetto di "grant non più ammesso", né un comando di pulizia dedicato. Unico aiuto: in Attivi, accanto al nome della regola compare "(rimossa)" se la regola non esiste più; il grant ricorda la regola da cui è nato, quindi non costa nulla.

## 7. Configurazione

### 7.1 Dove sta
- Le regole stanno in **AppConfig**, come un unico documento. Ogni salvataggio crea una versione numerata e immutabile.
- Ogni versione contiene **tutte** le regole: aggiungere, modificare o eliminare una regola crea una versione nuova completa. Questo dà salvataggio atomico, confronto tra versioni e ritorno a una versione precedente ripubblicandola.
- Vale sempre **l'ultima versione**. Non si usano i deployment di AppConfig.
- `api` tiene le regole in memoria e le rinfresca ogni 30 secondi circa: una regola salvata vale per tutti entro quel tempo, e una regola eliminata può ricevere richieste per lo stesso tempo.
- **Schema:** al documento è agganciato uno schema AppConfig: campi obbligatori, durata entro 12h, "privileged ⇒ approvazione manuale". AppConfig **non** lo applica alla creazione di una versione (spike C4: accetta anche contenuto non valido), ma lo applica su richiesta con `ValidateConfiguration`. Quindi: UI e CLI validano prima di pubblicare; `api` valida ogni versione nuova che legge e, se non è valida, la scarta, continua a usare l'ultima valida ed emette un avviso. Una versione valida scritta fuori da elevate (anche dalla console AWS) compare nella UI come le altre, con il suo autore.
- **Contenuto identico:** se il contenuto è uguale all'ultima versione, AppConfig non ne crea una nuova (spike C4).
- **Salvataggi concorrenti:** ogni salvataggio dichiara da quale versione parte e AppConfig lo rifiuta se nel frattempo ne è uscita un'altra. In quel caso la UI dice chi ha pubblicato e offre "Ricarica e riprova": il modulo resta compilato e si pubblica di nuovo. Se l'altra versione ha toccato la stessa regola, un avviso in più ("il tuo salvataggio sostituirà la sua modifica") con conferma. Nessuna fusione e nessun confronto affiancato.
- Non esistono bozze: "Salva" mostra il riepilogo di cosa cambia, chiede conferma e pubblica.
- Non esiste repo di config né pipeline.

### 7.2 Chi pubblica
Chi ha il PS `ElevateAdmin`, dalla UI o dalla CLI, **direttamente su AppConfig** con le proprie credenziali SSO. Nessuna Lambda nel percorso: l'autorizzazione la fa IAM.

### 7.3 Traccia
- CloudTrail registra per ogni versione autore (`AWSReservedSSO_ElevateAdmin_…/<username>`), ora e contenuto completo (spike S18).
- La Event history gratuita copre 90 giorni. Oltre serve un trail, che di norma l'organizzazione ha già. Il trail resta **facoltativo** e elevate non lo crea: un secondo trail sugli stessi eventi sarebbe a pagamento. `doctor` segnala se nell'account non ce n'è nessuno. Per chi è soggetto a NIS2 o DORA il trail è di fatto necessario, con validazione dei file di log e bucket in un account separato.
- **Data e autore di ogni versione li registra `api`.** AppConfig non li espone (spike C4). Quando `api` vede un numero di versione nuovo cerca in CloudTrail (Event history, gratuita) chi l'ha creata e quando, e lo salva in DynamoDB. Vale per ogni versione, da UI, CLI o console AWS; l'autore è quello verificato da AWS e resta oltre i 90 giorni. CloudTrail registra con qualche minuto di ritardo: fino ad allora la versione mostra "autore in verifica". Nessun costo aggiuntivo. La descrizione della versione non viene usata per l'autore.
- La vista Versioni config mostra l'elenco delle versioni con data e autore e permette il confronto tra due versioni. Elenco e contenuto passano da `api`, così li vede anche l'auditor.
- Un admin può aggiungere una regola per sé e usarla: è ammesso, purché resti traccia.

### 7.4 Export e import
`aws elevate admin export` e `import` salvano e ricaricano le regole da file: backup e replica di un'installazione.

### 7.5 Elenco dei permission set e degli account
- Letti da Identity Center e da Organizations con le credenziali dell'admin. Le letture sono gratuite.
- L'elenco dei PS (solo nomi e ARN) sta in una cache locale sul computer dell'admin, `~/.cache/aws-elevate/`, una per profilo, valida 10 minuti e con pulsante "aggiorna".
- Il contenuto di un PS ("Cosa contiene") si legge da AWS quando si espande il riquadro.
- Chi fa richieste non legge mai l'elenco: vede le regole, che contengono già nome e ARN.
- Una regola il cui PS non è più nell'elenco (cancellato in AWS) prende l'etichetta "PS non trovato" nella vista Regole e in `doctor`, senza chiamate in più. Un grant su quella regola fallisce con il motivo.

## 8. Sicurezza

### 8.1 Identità
- Ogni chiamata è firmata SigV4 con credenziali SSO. L'identità è solo quella della firma, mai un campo della richiesta.
- Resource policy di API Gateway: ammessi solo i ruoli sotto il path `aws-reserved/sso.amazonaws.com/` con nome `AWSReservedSSO_ElevateRequester_*` o `AWSReservedSSO_ElevateAdmin_*`, **con Deny esplicito per tutti gli altri** (spike S5: nello stesso account il solo Allow non basta).
- `api` confronta il RoleId della chiamata (`AROA…`) con quello reale dei due ruoli: un ruolo creato con lo stesso nome non passa.
- `ElevateRequester` permette solo di chiamare l'API e di leggere il parametro SSM con il suo indirizzo (§10.1). Chi non rientra in nessuna regola riceve 403. Anche approvatori e auditor devono averlo, oppure avere `ElevateAdmin`.
- Approvatore e auditor non sono ruoli IAM: `api` li riconosce dall'appartenenza ai gruppi, letta da Identity Center a ogni chiamata.

### 8.2 Permessi dei componenti

| Ruolo | Può | Non può |
|---|---|---|
| `api` | DynamoDB `grants`; lettura AppConfig e `ValidateConfiguration`; ricerca in CloudTrail (autore delle versioni); `StartExecution`, `SendTask*`, redrive; lettura Identity Store (utenti, gruppi) | Qualsiasi scrittura `sso` |
| `engine` | Lettura dei PS (per copiare l'origine); creazione, scrittura, assegnazione e cancellazione **solo su PS con tag `jit=true`**; DynamoDB `grants`; lettura AppConfig | Scrivere o assegnare PS senza tag `jit`; rimuovere il tag |
| State machine | Invocare `engine`; `PutEvents` sul bus | Tutto il resto |
| PS `ElevateAdmin` | Chiamare l'API e leggere il parametro con il suo indirizzo; pubblicare versioni di config su AppConfig; leggere PS, account, gruppi; leggere la history di CloudTrail; creare e leggere anteprime di aggiornamento dello stack (§12.3) | Applicare aggiornamenti dello stack; modificare IAM o Lambda; scrivere PS |

Lo scoping per tag è quello validato nello spike (S1, policy di riferimento `spike/policies/scoped-role.json`): `TagResource` condizionato, `UntagResource` negato, chiavi di tag ammesse elencate.

### 8.3 Prerequisito dichiarato

**Nel service account scrivono solo gli amministratori di Identity Center.** Chi ha IAM admin in quell'account può già assegnarsi qualsiasi PS con le API di Identity Center, quindi elevate non aggiunge potere a chi è già lì. Chi invece avesse permessi ampi su Lambda, DynamoDB o Step Functions senza essere admin di Identity Center potrebbe manomettere il servizio: per questo il prerequisito, e per questo lo stack SCP opzionale.

In più, sempre presente: resource policy sulla tabella `grants` con Deny esplicito per chi non è un ruolo del servizio (spike S17).

### 8.4 Stack SCP opzionale
`elevate-guardrails`, nel management account, per chi condivide il service account con altri team. Protegge le risorse `elevate-*` e le azioni `states:SendTask*` e `StartExecution` (spike S16, S21). Il servizio non ne verifica la presenza: nessun canary.

### 8.5 Cosa si è scelto di non avere, rispetto alla v1

| Rinuncia | Conseguenza accettata |
|---|---|
| Separazione builder / activator | Una compromissione del codice dell'engine concede accessi arbitrari. La v1 non copriva comunque la compromissione di deploy e dipendenze. L'engine non riceve input esterno |
| Validator e reconciler | Una modifica manuale a un PS `jit` (policy aggiunta, altro utente assegnato) non viene rilevata né annullata. Può farla solo chi è già admin di Identity Center. Resta in CloudTrail con autore e contenuto (spike S4) |
| Revisione obbligatoria della config | Un admin pubblica da solo. Resta la traccia. Chi ha `ElevateAdmin` può creare una regola per sé con approvazione automatica su qualsiasi PS esistente: il PS va dato solo a chi è già admin di Identity Center |
| SCP obbligatoria | Diventa il prerequisito di §8.3 |
| Privileged rilevato dal contenuto | Lo dichiara l'admin. Se sbaglia, o se il PS di origine viene allargato dopo, la regola resta com'è |
| Config ristretta che revoca i grant attivi | Restano fino a scadenza; l'admin li revoca a mano se vuole (§6.6) |

### 8.6 UI locale
`aws elevate ui` avvia un server su `127.0.0.1` che firma le chiamate con le credenziali SSO dell'utente. Il server non decide nulla: l'autorizzazione resta in AWS.

- Ascolto solo su `127.0.0.1`, porta casuale.
- Segreto generato a ogni avvio, scambiato con un cookie non leggibile da script.
- Controllo di `Host` e `Origin` su ogni richiesta; nessun CORS.
- Nessuna risorsa esterna caricata.
- Le credenziali restano nel processo locale e non arrivano al browser.
- Chiusura automatica dopo inattività.

## 9. Eventi e avvisi

### 9.1 Eventi
Ogni transizione del grant è un evento sul bus `elevate-events`, `source = elevate`, emesso dalla state machine con l'integrazione nativa verso EventBridge (con retry, e visibile nella history dell'execution).

| `detail-type` | Quando |
|---|---|
| `elevate.request.created` | Richiesta accettata |
| `elevate.request.approved` / `rejected` / `timed_out` / `cancelled` / `invalidated` | Esito della fase di attesa |
| `elevate.grant.activated` | Accesso utilizzabile |
| `elevate.grant.closed` | Fine grant, con motivo (`expired`, `revoked`, `failed`) e, per `revoked`, chi e perché |
| `elevate.grant.error` | Errore gestito; `reason = revoke_failed` per la revoca fallita |

Payload versionato (`schemaVersion`), con `eventId` deterministico (`<grantId>:<transizione>`), richiedente, approvatore, regola, gruppo che approva, PS di origine, account, inizio, fine, versione della config, motivo. Consegna *at-least-once*; idempotenza e ordinamento a carico del consumer. Campi nuovi solo in aggiunta. Consumer di altri account tramite resource policy del bus (spike S19); gli account ammessi si elencano in un parametro facoltativo dello stack.

### 9.2 Avvisi
Una regola EventBridge inoltra i soli eventi critici (`elevate.grant.error`) al topic SNS `elevate-alerts`, con testo leggibile prodotto dal trasformatore della regola. Nessun codice. L'oggetto dell'email non è impostabile in questo modo e resta quello predefinito di SNS (spike C7). L'email è un parametro dello stack; chiunque altro può iscriversi al topic.

**Richieste da approvare.** Un secondo topic, `elevate-approvals`, creato dallo stack senza iscritti, riceve le richieste manuali in attesa: chi vuole l'email si iscrive. Non distingue per regola, quindi chi è iscritto le riceve tutte; il testo indica chi chiede, cosa e quale gruppo deve approvare. Chi non si iscrive vede le richieste nella UI e in `aws elevate status`.

Da dichiarare nella documentazione: il topic generico va bene per i team piccoli; i team strutturati usano l'evento, che porta il gruppo che approva, con un proprio consumer sul bus o con il modulo Slack.

### 9.3 Slack (modulo opzionale)
Notifiche delle richieste e bottoni Approva / Rifiuta. È un consumer degli eventi come gli altri. Si installa come secondo stack facoltativo, `elevate-slack`, che si aggancia al bus e all'API. Identità dell'approvatore su Slack come in v1 §8.8 (email univoca, spike S22).

## 10. CLI

### 10.1 Installazione
```bash
uv tool install aws-elevate-cli && aws-elevate setup
```
`aws-elevate setup` non chiede parametri: usa la sessione SSO già aperta per trovare l'account in cui l'utente ha `ElevateRequester` o `ElevateAdmin`, legge l'indirizzo dell'API da un parametro SSM a nome fisso, crea l'alias `aws elevate` e il profilo. Con più sessioni SSO si indica `--profile`, come nella AWS CLI. `--profile` vale per tutti i comandi.

`uv` scarica da solo il Python che serve (la AWS CLI v2 non ne installa uno utilizzabile). In alternativa `pipx`.

### 10.2 Comandi

Pochi, con nomi semplici da ricordare. Tutti hanno `--help` e `--profile`.

| Comando | Descrizione |
|---|---|
| `aws elevate request [<regola>] --account <a> [--duration 45m] --reason "…"` | Crea la richiesta, attende, scrive il profilo. Senza argomenti mostra cosa si può chiedere |
| `aws elevate shell` | Apre una shell con il profilo del grant già attivo |
| `aws elevate status` | Grant e richieste dell'utente; per gli approvatori anche le richieste da approvare |
| `aws elevate revoke [<id> …]` | Revoca anticipata, di uno o più grant |
| `aws elevate approve <id>` / `--reject` | Per gli approvatori |
| `aws elevate ui` | UI locale |
| `aws-elevate setup` / `aws-elevate install` | Configurazione del client (§10.1) e installazione guidata dell'infrastruttura (§12.1) |
| `aws elevate admin export` / `import` | Regole da e verso file |
| `aws elevate admin doctor` | Controlla stack, permessi, config, avvisi e versioni; dice cosa manca |

### 10.3 Uso del grant
Un utente SSO entra in un account sempre attraverso un ruolo, e un profilo della CLI è la coppia account + ruolo: il grant è quindi un profilo diverso da quello abituale.

- Il profilo ha un nome stabile, `jit-<permission set>-<account>`, scritto dalla CLI e uguale da un grant all'altro.
- `aws elevate shell` evita `--profile`; in alternativa la CLI stampa la riga `export AWS_PROFILE=…`.
- In console il ruolo compare nel portale di accesso AWS.
- **"Apri in console":** a grant pronto (§6.3), la UI (vista Richiedi dopo l'invio, e Attivi) mostra un pulsante che apre la console AWS direttamente su quell'account e quel ruolo, con un link al portale di accesso: `<start URL>/#/console?account_id=<account>&role_name=<PS effimero>` (provato nello spike C12). Premuto prima che il grant sia pronto, il portale risponde "No access": per questo il pulsante resta disattivato fino alla verifica del client.
- **In console, a fine grant** (spike C12): dopo la scadenza le pagine danno accesso negato; dopo una revoca, per qualche secondo i dati si vedono ancora, poi la console risponde "security token invalid". In entrambi i casi la sessione console resta aperta ma non può fare nulla.

## 11. UI locale

### 11.1 Principi
- **Coerenza:** un nome e un colore per stato, uguali in ogni vista. Ogni lista ha la stessa struttura: ricerca, filtri rapidi, tabella, barra di selezione in basso.
- **Azioni distruttive:** conferma con elenco degli id e motivo obbligatorio. Dopo l'azione ogni riga mostra il proprio esito, mai un "fatto" generico.
- **Tempi relativi** ("38 min") con l'orario esatto al passaggio del mouse. **Id copiabili** con un clic.
- **Leggerezza:** pagine HTML generate dal server locale Python, pochissimo JavaScript, nessun framework, nessuna build con Node, nessuna risorsa esterna. Tutto dentro il pacchetto della CLI.
- **Liste:** 50 righe per pagina; la pagina nuova sostituisce la precedente; il dettaglio si carica solo all'apertura; un solo timer per tutti i countdown.

### 11.2 Viste

| Vista | Chi | Contenuto |
|---|---|---|
| **Richiedi** | Tutti | A sinistra ciò che l'utente può chiedere (le regole del suo gruppo). A destra un modulo uguale per ogni regola: account, durata, motivo. "Cosa otterrai" chiuso di default ed espandibile: policy AWS con link, tabella azioni / risorse della inline, JSON completo. Dopo l'invio: avanzamento e comando pronto |
| **Attivi** | Tutti (i propri); approvatori e admin (quelli che possono revocare) | Lista con caselle, "seleziona tutti", countdown, pulsante Revoca con conferma. Accanto alla regola, "(rimossa)" se non esiste più. Finché un grant è in attivazione o in revoca la pagina ne chiede lo stato con attese crescenti (2, 4, 8 secondi, poi ogni 8), poi smette |
| **In attesa** | Richiedente, approvatori | Una scheda per richiesta: motivo, contenuto del permesso, storico recente del richiedente. Approva / Rifiuta; Annulla sulle proprie |
| **Storico** | Tutti (i propri); admin e auditor (tutto) | Filtri, pagine, timeline del singolo grant a lato, export CSV |
| **Regole** | Admin | Sezione unica: tabella delle regole e modulo di una regola (§3.1), con ricerca del PS e "Cosa contiene" espandibile. Salva con riepilogo e conferma |
| **Versioni config** | Admin, auditor | Chi ha pubblicato cosa, confronto tra versioni, export / import |
| **Problemi** | Admin | Revoche e attivazioni fallite, con "accesso ancora utilizzabile per N min" e Riprova |
| **Aggiornamenti** | Admin | §12 |
| **Primo avvio** | Admin, solo finché non esiste nessuna regola | A tutto schermo, saltabile: procedura guidata per la prima regola (cosa, dove, chi, come) |

I mockup usati nella discussione sono bozze a bassa fedeltà in `.superpowers/brainstorm/` (non versionati).

### 11.3 Storico: peso e costo
- Si legge solo da DynamoDB, mai da log o CloudTrail.
- Letture per chiave (periodo, oppure utente) e paginate: mai una scansione dell'intera tabella.
- A schermo sempre dentro un periodo: predefinito ultimi 30 giorni, estendibile fino ai 13 mesi conservati.
- "Tutto" esiste solo come export, letto a pagine.
- Conservazione 13 mesi, poi cancellazione automatica (TTL).
- Ordine di grandezza stimato, non misurato: circa 1 KB per grant; 100.000 grant l'anno sono circa 100 MB.

## 12. Installazione, versioni, aggiornamenti

### 12.1 Installazione

| Passo | Cosa | Tempo stimato |
|---|---|---|
| 1 | "Launch Stack" nel delegated admin. Parametri: gruppo admin, gruppo che può fare richieste, email per gli avvisi; facoltativo il gruppo auditor | ~10 min di attesa |
| 2 | Conferma nell'email di SNS | 1 min |
| 3 | `uv tool install aws-elevate-cli && aws-elevate setup` | 2 min |
| 4 | `aws elevate ui` → procedura guidata per la prima regola → Salva | 5 min |

Lo stack trova da solo l'istanza di Identity Center. Come primo passo verifica region e account, e si ferma con un messaggio chiaro se la region non è quella di Identity Center o se l'account non è il delegated admin (spike C15: circa 40 s, create solo due risorse). Il controllo ha un timeout di 60 s: senza, se non risponde lo stack resta bloccato fino a un'ora. Misurato nello spike: stack in circa 150 s. Gli altri tempi sono stime.

Due modi di installare, entrambi documentati:

- **Guidato:** `aws-elevate install`, lanciato dall'admin con il proprio profilo. Trova la region di Identity Center e la mostra per conferma, controlla che l'account sia il delegated admin, fa scegliere i gruppi per nome da un elenco, chiede l'email degli avvisi, lancia lo stack CloudFormation, attende e alla fine esegue il setup del client. Sostituisce i passi 1 e 3.
- **Manuale**, per chi preferisce vedere ogni passo: "Launch Stack" o `aws cloudformation create-stack`. Qui i gruppi si indicano con il loro **id** (CloudFormation assegna i PS di servizio per id, non per nome): si copia dalla pagina del gruppo in Identity Center o con `aws identitystore get-group-id`. La documentazione indica come trovare la region di Identity Center prima di lanciare.

In entrambi i casi serve solo CloudFormation, già presente in AWS: nessun Terraform né altro strumento da installare.

**Distribuzione.** Il client si installa da PyPI (o dal repo Git). Template e codice delle Lambda stanno in bucket S3 pubblici delle release, uno per region supportata, perché CloudFormation vuole il pacchetto nella stessa region dello stack; li pubblica uno script a ogni release. Le letture di CloudFormation e Lambda dalla stessa region non costano. Da decidere alla prima release: come limitare il costo dei download da internet sul bucket pubblico (come minimo un allarme di budget).

### 12.2 Versioni
Client e infrastruttura hanno ciascuno la propria versione (`major.minor.patch`) e possono avanzare separatamente. Ognuno dichiara cosa gli serve dall'altro, come costante della release:

| Chi dichiara | Cosa |
|---|---|
| Infrastruttura | `minClient`: versione minima del client che accetta |
| Client | `minInfra`: versione minima dell'infrastruttura con cui sa parlare |

A ogni chiamata il client invia la sua versione e l'API risponde con la propria e con `minClient`. La combinazione è valida se entrambe le condizioni sono soddisfatte; altrimenti il client si ferma e mostra il comando per installare una versione compatibile. Nessuna tabella di compatibilità esterna.

Le release minori non introducono regressioni; quelle maggiori hanno note di migrazione. Ogni release è provata così: installa la precedente, apri un grant, aggiorna, verifica che scada e venga pulito.

### 12.3 Aggiornamenti
- **Controllo:** client e UI leggono le release dal repo pubblico e mostrano le novità, per client e infrastruttura, con tipo (sicurezza, correzione, novità) e descrizione. Disattivabile. Sono proposte solo le versioni del client compatibili con l'infrastruttura a cui si è collegati.
- **Client:** si aggiorna dalla UI o con `uv tool upgrade`.
- **Infrastruttura:** la UI **non** la aggiorna.
  - Pulsante "Verifica anteprima modifiche": crea un change set di CloudFormation, che non modifica nulla, e mostra l'elenco delle risorse che cambierebbero. `ElevateAdmin` può solo creare e leggere anteprime sul solo stack `elevate`, con template presi dal bucket ufficiale delle release.
  - Accanto, "mostra comando equivalente".
  - Per applicare: un comando mostrato dalla UI, da eseguire con il proprio ruolo admin dell'account. Esempio:

```bash
aws cloudformation update-stack \
  --profile <profilo admin> --region <region di Identity Center> \
  --stack-name elevate \
  --template-url https://<bucket delle release>/v2.4.0/elevate.yaml \
  --parameters ParameterKey=AdminGroup,UsePreviousValue=true \
               ParameterKey=RequesterGroup,UsePreviousValue=true \
               ParameterKey=AlertEmail,UsePreviousValue=true \
               ParameterKey=AuditorGroup,UsePreviousValue=true \
  --capabilities CAPABILITY_NAMED_IAM
```

Garanzie di un aggiornamento:

| Rischio | Garanzia |
|---|---|
| Grant attivi durante l'aggiornamento | Le execution in corso proseguono con la definizione con cui sono partite; la scadenza sta comunque nella policy. Il codice dell'engine invece è quello nuovo già dal passo successivo (spike C14): ogni release dell'engine deve accettare l'input delle execution avviate dalla release precedente, e il test di release lo verifica |
| Dati | Tabella e config non vengono mai sostituite né cancellate |
| Fallimento a metà | CloudFormation torna alla versione precedente |
| Client incompatibile | §12.2 |

## 13. Costi

Tutto gratuito o nell'ordine dei centesimi al mese per un team: Identity Center e le sue API sono gratuiti; Lambda, API Gateway, DynamoDB, Step Functions, EventBridge, AppConfig e SNS ai volumi previsti restano trascurabili. CloudTrail: basta la Event history gratuita (90 giorni); un trail è consigliato ma non richiesto.

Quote rilevanti, invariate dalla v1 §11.3: una modifica di ruolo per account alla volta (gestita con retry), 3 provisioning concorrenti, inline di 10.240 caratteri, 10 policy managed per ruolo.

## 14. Proposte in sospeso

Nessuna. Le proposte del redattore sono state discusse il 2026-11-15: approvate e spostate nelle sezioni di competenza (comprese P10, modulo Slack come secondo stack, e P11, account consumer come parametro), oppure ritirate (etichetta "non più ammesso" e comando di pulizia, §6.6).

## 15. Da validare (spike C, a design chiuso)

**Eseguito il 2026-11-15.** Esiti: tutte confermate tranne C4, falsa sul punto dello schema (AppConfig non lo applica alla creazione; vale la colonna "Se falsa", §7.1). Dettaglio nel report della fase C.

| # | Ipotesi | Se falsa |
|---|---|---|
| C1 | L'engine legge per intero un PS esistente (inline, managed, customer managed, boundary) e ne crea una copia `jit` equivalente con i permessi di §8.2 | Rivedere lo scoping di lettura |
| C2 | Revoca con riscrittura della scadenza + delete: tempo fino al primo "negato" e tempo totale | Solo delete (~17 s, già validato) |
| C3 | Il PS `ElevateAdmin` pubblica una versione su AppConfig e CloudTrail la attribuisce alla persona | Pubblicazione tramite `api` |
| C4 | Lettura dell'ultima versione ospitata senza deployment. Lo schema viene applicato già alla creazione della versione, anche da console. Il blocco sui salvataggi concorrenti funziona | Deployment AppConfig; se lo schema non blocca alla creazione, `api` scarta la versione non valida e avvisa |
| C5 | AppConfig non ha un tetto al numero di versioni conservate che ci riguardi | Pulizia delle versioni vecchie o copia in tabella |
| C6 | La state machine emette tutti gli eventi con l'integrazione nativa, con `eventId` deterministico | `PutEvents` dall'engine |
| C7 | Regola EventBridge → SNS con testo leggibile, senza codice | Lambda di formattazione |
| C8 | Redrive di una execution con revoca fallita riprende dal passo giusto | Comando di pulizia che avvia una nuova execution di sola revoca |
| C9 | Resource policy di API Gateway con due ruoli SSO ammessi e Deny esplicito; controllo del RoleId per entrambi | — |
| C10 | `ElevateAdmin` crea e legge un change set sul solo stack `elevate`, limitato ai template del bucket ufficiale, e non può eseguirlo | Solo comando manuale |
| C11 | `aws-elevate setup` trova account e API dalla sola sessione SSO | Chiedere l'account id |
| C12 | Scadenza e revoca viste dalla **console AWS**, non solo da CLI | Documentare la differenza |
| C13 | Sessione pari alla durata del grant (fino a 12h): scadenza e revoca restano effettive, da CLI e in console | Sessione fissa a 1h (valore validato in v1) |
| C14 | Aggiornamento dello stack con un grant attivo: l'execution prosegue e pulisce | Bloccare l'aggiornamento con grant attivi |
| C15 | Tempo reale di installazione da zero, guidata e manuale. CloudFormation vuole l'id del gruppo e non il nome. Lo stack si ferma entro il primo minuto su region o account sbagliati | Adeguare §12.1 |
| — | Aperte dalla v1: S6 non serve più (nessun validator). S8 non serve più (Windows escluso). S12 rimandata con i parametri |  |

## 16. Fase 2

- **Parametri** nei permessi (un PS per più perimetri, valore scelto alla richiesta).
- Approvazione automatica per chi è reperibile (PagerDuty / OpsGenie).
- Avvio programmato ed estensione di un grant attivo.
- Scorciatoia `aws elevate admin upgrade` per applicare gli aggiornamenti dell'infrastruttura.
- Gruppi con durate massime diverse sulla stessa regola: non richiesto.
- **Contesti regolati (NIS2, DORA):** accesso di emergenza (break-glass); riesame periodico delle regole con registrazione di chi l'ha fatto e quando; limite ai PS selezionabili nelle regole; avvisi alla pubblicazione di una versione di regole e alla modifica di un PS di origine.

Responsabilità dell'organizzazione, fuori da elevate: MFA e durata della sessione SSO, trail CloudTrail, politiche e riesami richiesti dalle norme.

## 17. Riferimenti

- [Temporary elevated access, IAM Identity Center](https://docs.aws.amazon.com/singlesignon/latest/userguide/temporary-elevated-access.html)
- [Quote di IAM Identity Center](https://docs.aws.amazon.com/singlesignon/latest/userguide/limits.html)
- [Service Authorization Reference, IAM Identity Center](https://docs.aws.amazon.com/service-authorization/latest/reference/list_awsiamidentitycenter.html)
- [Account access manager, IAM User Guide](https://docs.aws.amazon.com/IAM/latest/UserGuide/account-access-manager.html)
- [fivexl/terraform-aws-sso-elevator](https://github.com/fivexl/terraform-aws-sso-elevator)
- [aws-samples/iam-identity-center-team](https://github.com/aws-samples/iam-identity-center-team)
