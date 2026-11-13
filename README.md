# gh-runner

Um pool de runners próprios do GitHub Actions, compartilhado entre todos os repos privados da sua conta, sem precisar de organização.

Numa conta pessoal, o GitHub só deixa um runner se registrar num único repo. O `ghpool` contorna isso com runners _just-in-time_: ele acompanha os repos, e quando aparece um job na fila pedindo as labels do pool, registra um runner efêmero naquele repo, sobe um container que roda só esse job e o descarta em seguida. As vagas do pool (6 por padrão) atendem qualquer repo, por ordem de chegada dos jobs.

```
job na fila (qualquer repo) ──► ghpool ──► runner JIT no repo ──► container roda 1 job ──► sai
                                  ▲                                                       │
                                  └────────────────── vaga livre de novo ◄────────────────┘
```

## Como funciona

- **Descoberta dos repos.** O pool atende todos os repos privados e não arquivados que a credencial alcança, e relê a lista a cada 5 minutos. Repo novo entra sozinho. `REPOS_INCLUDE` e `REPOS_EXCLUDE` filtram por padrão (`bumaruf/*`, `bumaruf/site`).
- **Repos públicos ficam de fora.** Num repo público, um pull request de fork rodaria código nesta máquina. Só entram com `ALLOW_PUBLIC_REPOS=true`.
- **Consulta barata.** A cada 5 segundos, o pool lê os runs ativos de cada repo e os jobs deles com requisições condicionais (ETag). Quando nada mudou, o GitHub responde `304` e não desconta do limite de chamadas. Parado, o pool gasta perto de zero da cota; com atividade, só as respostas que mudaram contam. O intervalo se alarga sozinho com muitos repos, e o pool pausa se o GitHub pedir (limite primário ou secundário).
- **Um job, um container.** Cada job roda num container novo da imagem `runner/`, que termina com o job. Nada de um job sobra para o próximo, e o GitHub remove o registro do runner ao final.
- **Vagas e caches.** Cada vaga tem volumes próprios para `~/.cache` (Playwright e outros), o store do pnpm, o cache do npm e o tool cache do `setup-*`. Uma vaga roda um job por vez, então dois jobs nunca escrevem no mesmo cache ao mesmo tempo. Jobs de repos diferentes compartilham os caches de uma vaga ao longo do tempo.
- **Rede.** Cada vaga tem a própria rede do Docker; jobs em paralelo não se enxergam.
- **Sem estado.** A cada ciclo, o pool remonta o quadro a partir do Docker e do GitHub. Se o serviço reinicia, os jobs em andamento continuam, e os containers são adotados na volta.
- **Limpeza.** Container que termina sai do pool. Runner que subiu e ficou sem job (o run foi cancelado, por exemplo) é removido depois de `IDLE_TIMEOUT`, desde que o repo não tenha job na fila. A cada 5 minutos, o pool desregistra runners com o nome dele que ficaram offline sem container, como os que uma queda de energia deixaria.
- **Falhas.** Se o registro ou o container de um repo falha, o repo espera antes de tentar de novo, de 10 segundos até 5 minutos, e os outros repos seguem normalmente.
- **Labels.** Os runners têm `self-hosted`, `linux`, a arquitetura (`x64` ou `arm64`) e as de `POOL_LABELS` (`gh-runner` por padrão). O pool só pega jobs cujo `runs-on` caiba nessas labels.

## Usar num repo

Nos jobs do workflow:

```yaml
runs-on: ${{ fromJSON(vars.CI_RUNS_ON || '"ubuntu-latest"') }}
```

Sem a variável `CI_RUNS_ON`, o job roda no `ubuntu-latest` do GitHub. Para mandar o repo para o pool:

```bash
gh variable set CI_RUNS_ON --repo <owner/repo> --body '["self-hosted","gh-runner"]'
```

Para voltar aos runners do GitHub:

```bash
gh variable delete CI_RUNS_ON --repo <owner/repo>
```

Também dá para fixar `runs-on: [self-hosted, gh-runner]` direto no workflow.

O workflow `selftest` deste repo dispara 3 jobs ao mesmo tempo no pool e confere runner, caches, `setup-node` e `sudo`:

```bash
gh workflow run selftest.yml --repo <owner>/gh-runner
```

Como o pool ignora repos públicos por padrão, rode o `selftest` num fork privado deste repo, ou num pool com `ALLOW_PUBLIC_REPOS=true` restrito a ele por `REPOS_INCLUDE`.

## Credencial

O pool precisa registrar runners nos repos e ler os runs.

**GitHub App (recomendado).** Os tokens duram uma hora e alcançam só os repos onde a app está instalada.

1. Em _Settings → Developer settings → GitHub Apps → New GitHub App_: qualquer nome e homepage, _Webhook_ desmarcado, e as permissões de repositório:
   - _Administration_: Read and write;
   - _Actions_: Read-only;
   - _Metadata_: Read-only (já vem marcada).
2. Em _Where can this GitHub App be installed?_, _Only on this account_.
3. Gere uma _private key_ e salve como `secrets/github-app.pem`.
4. Instale a app na sua conta, em todos os repos ou nos escolhidos. Repos adicionados à instalação depois entram sozinhos no pool.
5. No `.env`, `GITHUB_APP_ID` com o _App ID_ ou o _Client ID_ da app.

**Token.** Em vez da app, um fine-grained token com _Administration_ (write) e _Actions_ (read) nos repos, em `GITHUB_TOKEN`. Ele não expira sozinho na mesma frequência e costuma alcançar mais do que precisa, por isso a app é melhor.

## Subir o pool

Num host com Docker e o plugin compose (num Ubuntu novo, rode antes `sudo ./host-setup.sh`):

```bash
cp .env.example .env    # e preencha
docker compose up -d --build
docker compose logs -f ghpool
```

O `compose` constrói a imagem dos runners (`ghpool-runner`) e a do serviço, e deixa o `ghpool` rodando com `restart: unless-stopped`. O serviço recebe o socket do Docker para criar os containers dos runners; os runners nunca recebem esse socket.

Para parar:

```bash
docker compose down
```

O serviço termina o ciclo em andamento antes de sair. Os jobs que estão rodando continuam e são adotados quando ele voltar.

Rode uma única instância por `POOL_NAME`. Hosts diferentes (um PC e uma VM, por exemplo) usam nomes diferentes e podem atender os mesmos repos ao mesmo tempo.

## Configuração

| Variável | Padrão | O que faz |
|---|---|---|
| `GITHUB_APP_ID` | | App ID ou Client ID da GitHub App. |
| `GITHUB_APP_PRIVATE_KEY_FILE` | | Chave privada da app (no compose, `/run/secrets/github-app.pem`). |
| `GITHUB_APP_INSTALLATION_ID` | | Só se a app tiver mais de uma instalação. |
| `GITHUB_TOKEN` | | Alternativa à app. |
| `POOL_NAME` | `ghpool` | Distingue pools no mesmo Docker e prefixa os nomes dos runners. |
| `POOL_SIZE` | `6` | Vagas, ou seja, jobs ao mesmo tempo. |
| `POOL_LABELS` | `gh-runner` | Labels extras dos runners, separadas por vírgula. |
| `RUNNER_CPUS` | `0` | Limite de CPU por job (`4`, `2.5`); `0` é sem limite. |
| `RUNNER_MEMORY` | `0` | Limite de memória por job (`8g`, `512m`); `0` é sem limite. |
| `POLL_INTERVAL` | `5s` | Intervalo entre consultas. |
| `IDLE_TIMEOUT` | `5m` | Quanto um runner sem job espera antes de sair. |
| `REPO_REFRESH` | `5m` | Intervalo para reler a lista de repos. |
| `REPOS_INCLUDE` | | Padrões de repos atendidos; vazio é todos. |
| `REPOS_EXCLUDE` | | Padrões de repos ignorados. |
| `ALLOW_PUBLIC_REPOS` | `false` | Atende também repos públicos. |
| `RUNNER_VERSION` | `2.338.2` | Versão de `actions/runner` da imagem (no compose). |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` ou `error`. |

`nproc` dentro de um job mostra todos os núcleos do host, mesmo com `RUNNER_CPUS`. Ferramentas que paralelizam por `nproc` podem precisar de um limite próprio.

## Limitações

- Os containers dos runners não têm Docker. Jobs com `services:`, `container:` ou `docker build` não rodam no pool; mantenha esses no `ubuntu-latest`.
- Entre o job entrar na fila e começar, passam alguns segundos: até um ciclo de consulta mais a subida do container (cerca de 15 segundos no total).
- Os jobs de repos diferentes compartilham os caches de uma vaga. Um job pode, em tese, envenenar um cache usado depois por outro repo. Como o pool só atende os seus repos privados, a troca vale pela velocidade.

## Atualizar a versão do runner

Troque `RUNNER_VERSION` (no `.env` ou no padrão do `compose.yaml` e do `runner/Dockerfile`) por uma versão de [actions/runner](https://github.com/actions/runner/releases) com pelo menos duas semanas, e:

```bash
docker compose up -d --build
```

Os próximos jobs já usam a imagem nova. O runner também se atualiza sozinho quando o GitHub exige uma versão mais nova.

## Desenvolvimento

```bash
go test -race ./...
go vet ./...
```

`internal/pool/plan.go` concentra as regras de decisão (o que remover, onde subir runners) numa função pura, coberta por `plan_test.go`. O resto é I/O: `internal/github` (REST com ETag, token de app) e `internal/docker` (Engine API pelo socket), só com a biblioteca padrão.
