#!/bin/sh
# Vhost da POC poc-vtex-seller-go no Caddy existente, pela API admin, sem reiniciar e sem tocar nas outras rotas.
# Uso: caddy-vhost.sh servidor | sitios | aplicar | remover
#   servidor  só leitura: nome do servidor HTTP que escuta na porta HTTPS (para se não houver exatamente um)
#   sitios    só leitura: código HTTP de cada host configurado no Caddy (rode antes e depois e compare)
#   aplicar   salva a config inteira em $BACKUP_DIR, confere que o @id não existe, insere a rota na posição 0
#             do servidor HTTPS (PUT em .../routes/0 insere, não substitui) e confere o resultado
#   remover   DELETE /id/poc-vtex: tira só a rota da POC
# Variáveis: ADMIN (localhost:2019), PORTA_HTTPS (443), VHOST_JSON (caddy-vhost.json ao lado do script),
#            BACKUP_DIR ($HOME; a pasta do repo é de root).
set -eu

ADMIN=${ADMIN:-localhost:2019}
PORTA_HTTPS=${PORTA_HTTPS:-443}
ID=poc-vtex
VHOST_JSON=${VHOST_JSON:-$(dirname "$0")/caddy-vhost.json}
BACKUP_DIR=${BACKUP_DIR:-$HOME}

servidor() {
  curl -fsS "$ADMIN/config/apps/http/servers/" | PORTA="$PORTA_HTTPS" python3 -c '
import json, os, sys
porta = ":" + os.environ["PORTA"]
s = [k for k, v in json.load(sys.stdin).items() if any(l.endswith(porta) for l in v.get("listen", []))]
if len(s) != 1:
    sys.exit(f"esperava 1 servidor em {porta}, achei {s}")
print(s[0])'
}

sitios() {
  curl -fsS "$ADMIN/config/" | python3 -c '
import json, sys
hosts = set()
def andar(x):
    if isinstance(x, dict):
        for k, v in x.items():
            if k == "host" and isinstance(v, list):
                hosts.update(h for h in v if isinstance(h, str) and "*" not in h and "{" not in h)
            andar(v)
    elif isinstance(x, list):
        for v in x:
            andar(v)
andar(json.load(sys.stdin))
print("\n".join(sorted(hosts)))' | while read -r h; do
    printf '%s %s\n' "$h" "$(curl -sk -o /dev/null -m 10 -w '%{http_code}' --resolve "$h:$PORTA_HTTPS:127.0.0.1" "https://$h:$PORTA_HTTPS/" || true)"
  done
}

total_rotas() {
  curl -fsS "$ADMIN/config/apps/http/servers/$1/routes" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))'
}

case "${1:-}" in
  servidor) servidor ;;
  sitios) sitios ;;
  aplicar)
    python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$VHOST_JSON"
    srv=$(servidor)
    if [ "$(curl -s -o /dev/null -w '%{http_code}' "$ADMIN/id/$ID")" = 200 ]; then
      echo "a rota $ID já existe; nada feito (use 'remover' antes para reaplicar)" >&2
      exit 1
    fi
    copia="$BACKUP_DIR/caddy-antes-$ID-$(date +%Y%m%d-%H%M%S).json"
    curl -fsS "$ADMIN/config/" > "$copia"
    antes=$(total_rotas "$srv")
    curl -fsS -X PUT "$ADMIN/config/apps/http/servers/$srv/routes/0" \
      -H 'Content-Type: application/json' --data-binary @"$VHOST_JSON" >/dev/null
    depois=$(total_rotas "$srv")
    primeira=$(curl -fsS "$ADMIN/config/apps/http/servers/$srv/routes/0" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("@id"))')
    echo "servidor=$srv rotas $antes -> $depois, posição 0 = $primeira, cópia da config anterior: $copia"
    [ "$depois" -eq $((antes + 1)) ] && [ "$primeira" = "$ID" ]
    ;;
  remover)
    curl -fsS -X DELETE "$ADMIN/id/$ID" >/dev/null && echo "rota $ID removida"
    ;;
  *)
    echo "uso: $0 servidor|sitios|aplicar|remover" >&2
    exit 2
    ;;
esac
