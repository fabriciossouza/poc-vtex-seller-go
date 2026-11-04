<?php
// Contract test for the ERP mock: php tests/run.php http://127.0.0.1:8081
declare(strict_types=1);

$base = $argv[1] ?? 'http://127.0.0.1:8081';
$failures = 0;

function call(string $method, string $path, ?array $body = null, array $headers = []): array
{
    global $base;
    $opts = ['method' => $method, 'ignore_errors' => true, 'header' => "Content-Type: application/json\r\n"];
    foreach ($headers as $name => $value) {
        $opts['header'] .= "$name: $value\r\n";
    }
    if ($body !== null) {
        $opts['content'] = json_encode($body);
    }
    $raw = file_get_contents($base . $path, false, stream_context_create(['http' => $opts]));
    preg_match('#HTTP/\S+ (\d+)#', $http_response_header[0], $m);
    return [(int) $m[1], json_decode((string) $raw, true) ?? [], $http_response_header];
}

function check(string $name, bool $ok): void
{
    global $failures;
    echo ($ok ? 'ok   ' : 'FAIL ') . $name . "\n";
    $failures += $ok ? 0 : 1;
}

$order = ['orderId' => 'o-1', 'marketplaceOrderId' => 'm-1', 'items' => [['sku' => '6', 'quantity' => 2, 'priceCents' => 2499]]];

call('POST', '/admin/reset');
[$status] = call('GET', '/healthz');
check('healthz answers 200', $status === 200);

[$status, $first] = call('POST', '/orders', $order, ['Idempotency-Key' => 'o-1']);
check('new order is created with 201', $status === 201 && ($first['status'] ?? '') === 'registered');

[$status, $again, $hdr] = call('POST', '/orders', $order, ['Idempotency-Key' => 'o-1']);
check('same key replays the same answer with 200', $status === 200 && $again['erpOrderId'] === $first['erpOrderId']);
check('replay is flagged in a header', in_array('Idempotent-Replay: true', $hdr, true));

[, $list] = call('GET', '/admin/orders');
check('replay did not create a second order', $list['count'] === 1);

[$status] = call('POST', '/orders', array_merge($order, ['marketplaceOrderId' => 'm-2']), ['Idempotency-Key' => 'other']);
check('another key for an existing orderId is 409', $status === 409);

[$status] = call('POST', '/orders', $order);
check('missing Idempotency-Key is 400', $status === 400);

[$status] = call('POST', '/orders', ['orderId' => 'x', 'items' => []], ['Idempotency-Key' => 'x']);
check('invalid order is 422', $status === 422);

call('PUT', '/admin/config', ['errorRate' => 1, 'latencyMs' => 0]);
[$status] = call('POST', '/orders', array_merge($order, ['orderId' => 'o-3']), ['Idempotency-Key' => 'o-3']);
check('errorRate 1 answers 503', $status === 503);
[, $list] = call('GET', '/admin/orders');
check('a failed call stores nothing', $list['count'] === 1);

call('PUT', '/admin/config', ['errorRate' => 0, 'latencyMs' => 300]);
$start = microtime(true);
[$status] = call('POST', '/orders', array_merge($order, ['orderId' => 'o-3']), ['Idempotency-Key' => 'o-3']);
check('latency is applied and the order succeeds', $status === 201 && microtime(true) - $start >= 0.3);

[$status] = call('PUT', '/admin/config', ['errorRate' => 5, 'latencyMs' => 0]);
check('out-of-range config is 400', $status === 400);

call('POST', '/admin/reset');
[, $list] = call('GET', '/admin/orders');
[, $cfg] = call('GET', '/admin/config');
check('reset clears orders and config', $list['count'] === 0 && $cfg['errorRate'] === 0 && $cfg['latencyMs'] === 0);

exit($failures === 0 ? 0 : 1);
