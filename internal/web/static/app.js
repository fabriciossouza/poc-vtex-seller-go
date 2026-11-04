"use strict";

// Every string that comes from the server is written with textContent, never
// as HTML: order ids and failure reasons are not trusted.

const $ = (id) => document.getElementById(id);
const buttons = Array.from(document.querySelectorAll("button"));
const breakerNames = { closed: "fechado", open: "aberto", "half-open": "meio-aberto" };
let latest = null;
let pausedUntil = 0;

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function say(text, isError) {
  const notice = $("notice");
  notice.textContent = text;
  notice.className = isError ? "level-error" : "level-ok";
}

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  let data = null;
  try {
    data = await res.json();
  } catch (_) {
    data = null;
  }
  if (res.status === 429) {
    pausedUntil = Date.now() + 5000;
    throw new Error("Muitas requisições deste IP. Aguarde alguns segundos.");
  }
  if (!res.ok) {
    throw new Error((data && data.error && data.error.message) || "Erro " + res.status);
  }
  return data;
}

function newId(prefix) {
  const bytes = new Uint8Array(4);
  crypto.getRandomValues(bytes);
  return prefix + "-" + Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

async function run(action) {
  buttons.forEach((b) => (b.disabled = true));
  try {
    await action();
  } catch (err) {
    say(err.message, true);
  } finally {
    buttons.forEach((b) => (b.disabled = false));
    await refresh();
  }
}

async function placeAndFulfill(prefix) {
  const id = newId(prefix);
  await api("POST", "/demo/orders", { marketplaceOrderId: id, sku: "6", quantity: 1 });
  await api("POST", "/demo/orders/" + id + "/fulfill");
}

$("btn-dup").addEventListener("click", () =>
  run(async () => {
    const id = newId("DUP");
    const first = await api("POST", "/demo/orders", { marketplaceOrderId: id, sku: "6", quantity: 1 });
    const second = await api("POST", "/demo/orders", { marketplaceOrderId: id, sku: "6", quantity: 1 });
    say(
      second.duplicate && second.sellerOrderId === first.sellerOrderId
        ? "O seller devolveu o mesmo pedido (" + second.sellerOrderId + "): existe um único pedido."
        : "Resultado inesperado: o segundo envio não foi tratado como duplicado.",
      !second.duplicate,
    );
  }),
);

$("btn-down").addEventListener("click", () =>
  run(async () => {
    await api("POST", "/demo/erp", { failing: true });
    await placeAndFulfill("ERP");
    say("ERP derrubado e pedido enviado. Acompanhe as tentativas, o breaker abrindo e a mensagem indo para a DLQ.");
  }),
);

$("btn-up").addEventListener("click", () =>
  run(async () => {
    await api("POST", "/demo/erp", { failing: false });
    say("ERP religado. Espere o breaker voltar a testar (a contagem aparece no quadro) e reprocesse a DLQ.");
  }),
);

$("btn-replay").addEventListener("click", () =>
  run(async () => {
    if (latest && latest.breaker.state === "open") {
      const seconds = Math.ceil(latest.breaker.retryInMs / 1000);
      say("O circuit breaker ainda está aberto; ele volta a testar em " + seconds + " s. Tente de novo em seguida.", true);
      return;
    }
    const res = await api("POST", "/demo/dlq/replay");
    say("Reprocessadas: " + res.replayed + ". Mantidas na DLQ: " + res.failed + ". Descartadas: " + res.discarded + ".");
  }),
);

$("btn-happy").addEventListener("click", () =>
  run(async () => {
    await placeAndFulfill("OK");
    say("Pedido enviado. Acompanhe a linha do tempo até o rastreio chegar ao marketplace.");
  }),
);

$("btn-reset").addEventListener("click", () =>
  run(async () => {
    await api("POST", "/demo/reset");
    say("Demo resetada.");
  }),
);

function renderStatus(state) {
  const breaker = $("breaker-state");
  breaker.textContent = breakerNames[state.breaker.state] || state.breaker.state;
  breaker.className = "state-" + state.breaker.state;
  let detail = state.breaker.consecutiveFailures + " falhas seguidas";
  if (state.breaker.retryInMs > 0) detail += " · testa de novo em " + Math.ceil(state.breaker.retryInMs / 1000) + " s";
  $("breaker-detail").textContent = detail;

  const erp = $("erp-state");
  erp.textContent = state.erp.failing ? "fora do ar" : "no ar";
  erp.className = state.erp.failing ? "level-error" : "level-ok";

  $("dlq-count").textContent = String(state.dlq.length);
  $("stock-list").textContent = state.stock.map((s) => "SKU " + s.sku + ": " + s.available).join(" · ");
}

function renderOrders(orders) {
  const box = $("orders");
  if (orders.length === 0) {
    box.replaceChildren(el("p", "empty", "Nenhum pedido ainda. Escolha um cenário."));
    return;
  }
  box.replaceChildren(
    ...orders.map((order) => {
      const card = el("div", "order");
      const title = el("h3", "", "Pedido do marketplace " + order.marketplaceOrderId);
      title.append(el("span", "badge", order.status));
      if (order.trackingCode) title.append(el("span", "badge", "rastreio " + order.trackingCode));
      const list = el("ul", "events");
      for (const e of order.events) {
        const item = el("li", "level-" + e.level);
        item.append(
          el("time", "", new Date(e.at).toLocaleTimeString("pt-BR")),
          el("span", "src", e.source),
          document.createTextNode(e.message),
        );
        list.append(item);
      }
      card.append(title, list);
      return card;
    }),
  );
}

function renderDLQ(letters) {
  const box = $("dlq");
  if (letters.length === 0) {
    box.replaceChildren(el("p", "empty", "A DLQ está vazia."));
    return;
  }
  box.replaceChildren(
    ...letters.map((l) => {
      const card = el("div", "letter");
      card.append(
        el("strong", "", "Pedido " + l.orderId + " · " + l.attempts + " tentativas"),
        el("div", "level-error", l.reason),
        el("small", "empty", "falhou em " + new Date(l.failedAt).toLocaleTimeString("pt-BR")),
      );
      return card;
    }),
  );
}

async function refresh() {
  if (Date.now() < pausedUntil) return;
  try {
    latest = await api("GET", "/demo/state");
    renderStatus(latest);
    renderOrders(latest.orders);
    renderDLQ(latest.dlq);
  } catch (err) {
    say(err.message, true);
  }
}

refresh();
setInterval(refresh, 1000);
