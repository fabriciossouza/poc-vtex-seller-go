# Contratos internos

Contratos entre o seller e os servicos que ele chama: ERP (SAP-Adapter), TMS e a fila. Sao os contratos que os mocks externos devem seguir. Os contratos do protocolo VTEX estao em `testdata/FONTES.md`.

Regras comuns dos clientes HTTP:

- Timeout padrao de 5 s por chamada (configuravel pelo `*http.Client` injetado).
- Corpo e resposta em JSON, `Content-Type: application/json`.
- Status 2xx e sucesso. Status 5xx e erro transitorio (o worker repete). Qualquer outro e erro permanente (nao repete). Erro de conexao e timeout tambem sao transitorios.
- Chamadas que criam algo levam o header `Idempotency-Key` com o id do pedido do seller. O servico deve devolver o mesmo resultado para a mesma chave.

## ERP (servico adaptador)

`POST {base}/orders`

```json
{
  "orderId": "959311095",
  "marketplaceOrderId": "1138342255777-01",
  "items": [{ "sku": "6", "quantity": 2, "priceCents": 2499 }]
}
```

- `201` ou `200`: pedido registrado.
- `409`: o ERP ja tem esse pedido; o cliente trata como sucesso.
- `5xx`: indisponivel (transitorio). `4xx`: pedido recusado (permanente).

## TMS

`POST {base}/quotes`: corpo `{"items":[{"sku":"6","quantity":2}]}`, resposta `200`:

```json
{ "priceCents": 1099, "estimateHours": 168 }
```

`POST {base}/shipments`: corpo `{"orderId","marketplaceOrderId","items":[{"sku","quantity"}]}` com `Idempotency-Key`, resposta `200` ou `201`:

```json
{ "trackingCode": "TRK-9" }
```

Resposta sem `trackingCode` e tratada como erro.

## Marketplace (seller para marketplace)

`POST {marketplaceServicesEndpoint}/pvt/orders/{marketplaceOrderId}/invoice`, corpo no formato de `testdata/invoice_request.json`. O `invoiceNumber` e `INV-{orderId}`, estavel por pedido, entao uma notificacao repetida e a mesma nota; `409` e tratado como sucesso.

O `marketplaceServicesEndpoint` vem no body do placement, controlado por quem chama. O cliente so conecta em hosts da allowlist (`AllowedHosts`, `host[:port]`), nao segue redirect e so aceita `http`/`https`. Lista vazia recusa tudo.

## Fila (Redis Streams)

- Stream de eventos: campos `type` e `order_id`. Consumer group com ack; mensagem sem ack por mais de `ClaimIdle` pode ser assumida por outro consumidor do grupo.
- Ack e rejeicao removem a mensagem do stream.
- DLQ: stream separado, campos `type`, `order_id`, `reason`, `attempts`, `failed_at`, `source_id`. Mensagem malformada vai para a DLQ com `attempts` 0.
- `Discard` remove da DLQ depois do reprocessamento.

## Resiliencia

- Retry: backoff exponencial (`BaseDelay` dobra a cada falha, limitado por `MaxDelay`) com jitter entre 50% e 100% do atraso. So repete erro transitorio, ate `MaxAttempts`.
- Circuit breaker: abre apos `FailureThreshold` falhas transitorias seguidas, rejeita chamadas por `OpenTimeout`, depois deixa uma chamada de teste passar (meio-aberto). Erro permanente (4xx) conta como dependencia saudavel.
- O breaker e unico para o despacho inteiro (ERP, TMS e notificacao ao marketplace).
- DLQ guarda o motivo e o numero de tentativas; o replay passa pelo mesmo retry e breaker.
