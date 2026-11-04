# Fontes dos payloads (contrato seller externo VTEX)

Origem: especificacoes OpenAPI oficiais da VTEX (repo github.com/vtex/openapi-schemas, a mesma fonte que gera developers.vtex.com). Valores monetarios em centavos. Dados todos ficticios e coerentes: SKU `6`, pedido do marketplace `1138342255777-01`, pedido do seller `959311095`.

Referencia geral: https://developers.vtex.com/docs/api-reference/marketplace-protocol-external-seller-fulfillment e https://developers.vtex.com/docs/guides/external-seller-integration-connector

## Marketplace -> seller (o servico Go implementa). Base: `https://{fulfillmentEndpoint}`, registrado no admin do marketplace.

| Endpoint | Metodo e caminho | Doc oficial | Obrigatorios | Observacoes |
|---|---|---|---|---|
| Simulacao | POST `/pvt/orderForms/simulation` | https://developers.vtex.com/docs/api-reference/marketplace-protocol-external-seller-fulfillment#post-/pvt/orderForms/simulation | req: `postalCode` (schema) e itens com `id`,`quantity`,`seller`. resp: `country`,`items`,`logisticsInfo`,`postalCode` | `sc` (canal de venda) vai no body; query string como `?sc=`/`an=` NAO e definida no protocolo: o seller as inclui no proprio `fulfillmentEndpoint` cadastrado (doc diz isso). Na simulacao de vitrine/indexacao `country`/`postalCode` podem faltar (e `isCheckedIn`); sem endereco no checkout o estoque e lido como zero. Timeout do marketplace: 2,5 s. Resp. e req. adaptados do exemplo oficial (valores trocados). |
| Order placement | POST `/pvt/orders` | ...#post-/pvt/orders | schema sem `required` formal. Headers listados como obrigatorios: `authorization` (`VTEX key=".." token=".."`), `content-length`, `accept`, `content-type`, `x-vtex-operation-id`, `x-forwarded-proto`, `x-forwarded-for`, `x-vtex-cache-client-bypass`, `traceparent`, `accept-encoding` | Body e resposta sao OBJETO unico no exemplo oficial, NAO array como o enunciado supunha (nenhum array na spec). Os arquivos `order_placement_*` seguem o objeto. `paymentData` vem `null` no exemplo (seller que processa pagamento usa `merchantName`). |
| Authorize fulfillment | POST `/pvt/orders/{sellerOrderId}/fulfill` | ...#post-/pvt/orders/-sellerOrderId-/fulfill | texto: so `marketplaceOrderId`. | `sellerOrderId` = `orderId` devolvido no placement. Inconsistencia da doc: o schema oficial reaproveita o do cancelamento (exige `marketplaceOrderGroup`,`cancellationRequestId`,`cancellationRequestDate`,`reason`,`requestedByUser`) e o exemplo tem esses campos; o texto diz que so vai `marketplaceOrderId`. O arquivo traz `marketplaceOrderId`, `marketplaceOrderGroup` e `paymentData` (do exemplo); aceitar o resto como opcional. Resposta (`date`,`marketplaceOrderId`,`orderId`,`receipt`) do exemplo. |
| Cancelamento | POST `/pvt/orders/{orderId}/cancel` | ...#post-/pvt/orders/-orderId-/cancel | `marketplaceOrderId`,`marketplaceOrderGroup`,`cancellationRequestId`,`cancellationRequestDate`,`reason`,`requestedByUser` | Existe. Resp. vazia = avaliando; resp. com `marketplaceOrderId`,`orderId`,`date`,`receipt` = confirma; recusar = enviar invoice. |

## Seller -> marketplace. Base: `https://{marketplaceServicesEndpoint}` (vem no campo `marketplaceServicesEndpoint` do placement).

| Endpoint | Metodo e caminho | Doc oficial | Obrigatorios | Observacoes |
|---|---|---|---|---|
| Enviar nota (protocolo) | POST `/pvt/orders/{marketplaceOrderId}/invoice` | https://developers.vtex.com/docs/api-reference/marketplace-protocol-external-seller-marketplace | `type` (`Output` venda, `Input` devolucao), `invoiceNumber`, `items[]` (`id`,`quantity`,`price`) | E ESTE o caminho do protocolo de seller externo. Arquivos `invoice_*`. Request montado do schema (a spec nao traz exemplo). Rastreio posterior: POST `.../invoice/{invoiceNumber}` (`courier`,`trackingNumber`,`trackingUrl`,`dispatchedDate`); status: POST `.../invoice/{invoiceNumber}/tracking` (`isDelivered`). Seller cancelar no marketplace: POST `/pvt/orders/{marketplaceOrderId}/cancel` (`reason`). |
| Nota via OMS | POST `/api/oms/pvt/orders/{orderId}/invoice` | https://developers.vtex.com/docs/api-reference/orders-api | `type`,`issuanceDate`,`invoiceNumber`,`invoiceValue` (string),`items` | API de OMS comum (exige conta VTEX/appKey, permissao "Notify invoice"), nao o protocolo de seller externo. Exemplo adaptado do oficial sem `embeddedInvoice`. |

## Divergencias dos caminhos propostos
- Item 5: para seller externo o caminho e `POST {marketplaceServicesEndpoint}/pvt/orders/{marketplaceOrderId}/invoice`, nao `/api/oms/pvt/orders/{orderId}/invoice` (esse e do OMS). Ambos tem request salvo.
- Item 4: cancelamento confirmado em `/pvt/orders/{orderId}/cancel`.
- Nao verificado: `an=` (nenhuma ocorrencia nas specs); `sc` so aparece no body da simulacao.
