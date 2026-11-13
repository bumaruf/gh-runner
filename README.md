# gh-runner

Runners próprios do GitHub Actions para qualquer repo, rodando em containers Docker numa VM (pensado para a Ampere Always Free da Oracle Cloud). A ideia é usar os runners hospedados do GitHub por padrão e trocar para estes quando os minutos gratuitos do mês acabam.

- `gh-runner`: roda na sua máquina, comanda o host por SSH e pede os tokens ao GitHub.
- `oracle-launch.sh`: roda na sua máquina, cria a VM na Oracle tentando de novo até haver capacidade.
- `host/`: o que vai para o host, em `/opt/gh-runner`.
  - `Dockerfile`: a imagem oficial `ghcr.io/actions/actions-runner` (amd64 e arm64), numa versão fixa, com o script de entrada.
  - `entrypoint.sh`: dentro do container, sobe o runner registrado, ou registra e remove o registro.
  - `runnerctl`: no host, cria e remove um container por runner.
  - `host-setup.sh`: prepara um Ubuntu novo (Docker, swap, rotação de logs, limpeza semanal).

## Como funciona

- **Um runner por repo.** Numa conta pessoal do GitHub, um runner só se registra num repo. Por isso cada runner é um container `ghr-<owner>--<repo>-<n>`, e um mesmo host atende vários repos com um ou mais runners cada.
- **Tokens.** O `gh-runner` gera o token de registro (ou de remoção) com `gh api` na sua máquina e o passa pelo SSH direto para o stdin do container. O token vale 1 hora e só serve para isso. Nenhum token fica no host, na imagem ou no repo.
- **Volumes.** Cada runner guarda a configuração (`.runner`, `.credentials`, `.credentials_rsaparams`) num volume, e o registro sobrevive a reinícios. Também tem, por runner, caches de `~/.cache` (Playwright e outros), do store do pnpm, do npm e do tool cache do `setup-*`.
- **Rede.** Cada container tem a própria rede do Docker. Jobs que usam porta fixa não se cruzam.
- **Isolamento.** Os containers não montam o socket do Docker nem pastas do host. Jobs que dependem de `services:` ou de `docker build` não rodam nesses runners.
- **Labels.** `self-hosted`, `Linux`, a arquitetura (`ARM64` na Ampere) e `oracle`. Para mudar a label extra, defina `GH_RUNNER_LABELS` em `/etc/gh-runner.env` no host e rode `gh-runner upgrade`.

Os comandos abaixo rodam da raiz deste repo, com o `gh` autenticado numa conta com admin nos repos atendidos.

## Usar num repo

Nos jobs do workflow:

```yaml
runs-on: ${{ fromJSON(vars.CI_RUNS_ON || '"ubuntu-latest"') }}
```

Sem a variável `CI_RUNS_ON`, o job roda no `ubuntu-latest` do GitHub. Quando os minutos acabarem:

```bash
./gh-runner use <owner/repo> self
```

Isso grava `CI_RUNS_ON=["self-hosted","oracle"]` no repo. Na virada do mês, volte:

```bash
./gh-runner use <owner/repo> hosted
```

O repo precisa ter runners registrados (abaixo) antes de usar `self`.

## Criar a VM na Oracle

Uma `VM.Standard.A1.Flex` (Ampere, arm64) com Ubuntu 24.04 aarch64, 2 OCPU, 12 GB de RAM e 100 GB de disco cabe no Always Free. A Oracle costuma responder "Out of capacity" para esse shape, e a saída é tentar de novo até conseguir. O `oracle-launch.sh` faz isso com a OCI CLI configurada em `~/.oci/config`:

```bash
OCI_CLI_PROFILE=DEFAULT \
OCI_SUBNET_ID=ocid1.subnet... \
OCI_IMAGE_ID=ocid1.image... \
./oracle-launch.sh
```

- A subnet precisa ser pública, numa VCN com internet gateway e rota `0.0.0.0/0`.
- A imagem é a `Canonical-Ubuntu-24.04-aarch64-*` da região.
- Opcionais: `OCI_COMPARTMENT_ID` (padrão: a tenancy do perfil), `OCI_AD` (padrão: o primeiro AD), `OCPUS` (2), `MEMORY_GB` (12), `BOOT_GB` (100), `NAME` (`gh-runner`), `RETRY_SECONDS` (60), `SSH_PUBLIC_KEY` (`~/.ssh/gh-runner-oracle_ed25519.pub`).
- Com mais de uma conta, rode um por conta, cada um com seu `OCI_CLI_PROFILE`.

O script para quando a VM fica `RUNNING`, mostra o IP público e avisa com `notify-send`. Para deixar rodando sem o terminal:

```bash
setsid nohup env OCI_CLI_PROFILE=DEFAULT OCI_SUBNET_ID=... OCI_IMAGE_ID=... ./oracle-launch.sh > launch.log 2>&1 < /dev/null &
```

**Recuperação de instância ociosa.** Numa conta só Always Free, a Oracle pode desligar uma instância que passa 7 dias com CPU, rede e memória abaixo de 20%. Um runner parado fica assim. Para evitar, converta a conta para Pay As You Go: o uso dentro do Always Free continua sem custo e a regra deixa de valer.

## Acessar o host

Por SSH, pelo alias `gh-runner` do `~/.ssh/config` (ou outro destino em `GH_RUNNER_HOST`):

```
Host gh-runner
  HostName <ip público da VM>
  User ubuntu
  IdentityFile ~/.ssh/gh-runner-oracle_ed25519
```

A VM só precisa de saída para a internet. Nenhuma porta além do SSH fica aberta.

## Preparar o host

```bash
./gh-runner setup
```

Copia `host/` para `/opt/gh-runner`, instala o Docker, cria 4 GB de swap e constrói a imagem.

## Adicionar runners a um repo

```bash
./gh-runner add <owner/repo> 2
./gh-runner add <owner/outro-repo>
```

Cada `add` cria runners novos, numerados depois dos que o repo já tem. Com 2 OCPU e 12 GB, dois runners rodando ao mesmo tempo é um bom limite para o host todo.

## Ver o estado

```bash
./gh-runner list
./gh-runner logs <owner/repo> 1
```

O `list` mostra os containers no host e, para cada repo, os runners como o GitHub os vê (`online`, `offline`, `busy`).

## Remover

```bash
./gh-runner remove <owner/repo> 2   # só o runner 2
./gh-runner remove <owner/repo>     # todos do repo
```

Tira o registro no GitHub e apaga o container, a rede e os volumes.

## Atualizar a versão do runner

A versão fica fixa no `FROM` de `host/Dockerfile`. Troque a tag por uma versão de [actions/runner](https://github.com/actions/runner/releases) com pelo menos duas semanas e rode:

```bash
./gh-runner upgrade
```

O runner também se atualiza sozinho quando o GitHub publica uma versão nova. Essa atualização some quando o container é recriado, e aí ele se atualiza de novo.

## Registrar de novo

O GitHub remove um runner que passa 14 dias offline. Se isso acontecer, remova e adicione de novo:

```bash
./gh-runner remove <owner/repo> 1
./gh-runner add <owner/repo>
```
