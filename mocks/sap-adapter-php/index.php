<?php
// ERP mock ("SAP-Adapter") following docs/contratos-internos.md.
// Run with the built-in server: php -S 0.0.0.0:8080 index.php
declare(strict_types=1);

const MAX_BODY_BYTES = 1048576;

function stateFile(): string
{
    return getenv('STATE_FILE') ?: sys_get_temp_dir() . '/erp-mock-state.json';
}

function defaultState(): array
{
    return ['config' => ['errorRate' => 0, 'latencyMs' => 0], 'orders' => [], 'seq' => 0];
}

// Each request is a fresh PHP process, so state lives in a file guarded by a lock.
function withState(callable $fn)
{
    $fh = fopen(stateFile(), 'c+');
    flock($fh, LOCK_EX);
    $raw = stream_get_contents($fh);
    $state = $raw !== '' && $raw !== false ? json_decode($raw, true) : defaultState();
    $result = $fn($state);
    ftruncate($fh, 0);
    rewind($fh);
    fwrite($fh, json_encode($state));
    flock($fh, LOCK_UN);
    fclose($fh);
    return $result;
}

function respond(int $status, array $body, array $headers = []): never
{
    http_response_code($status);
    header('Content-Type: application/json');
    foreach ($headers as $header) {
        header($header);
    }
    echo json_encode($body), "\n";
    exit;
}

function jsonBody(): ?array
{
    $raw = file_get_contents('php://input', false, null, 0, MAX_BODY_BYTES);
    $data = json_decode((string) $raw, true);
    return is_array($data) ? $data : null;
}

function handleOrder(): never
{
    $config = withState(fn(array &$s) => $s['config']);
    if ($config['latencyMs'] > 0) {
        usleep($config['latencyMs'] * 1000);
    }
    if ($config['errorRate'] > 0 && mt_rand() / mt_getrandmax() < $config['errorRate']) {
        respond(503, ['error' => 'ERP indisponivel (falha injetada)']);
    }

    $key = $_SERVER['HTTP_IDEMPOTENCY_KEY'] ?? '';
    if ($key === '') {
        respond(400, ['error' => 'Idempotency-Key is required']);
    }
    $order = jsonBody();
    if ($order === null || ($order['orderId'] ?? '') === '' || !is_array($order['items'] ?? null) || $order['items'] === []) {
        respond(422, ['error' => 'orderId and at least one item are required']);
    }

    [$status, $body, $headers] = withState(function (array &$s) use ($key, $order) {
        if (isset($s['orders'][$key])) {
            return [200, $s['orders'][$key]['response'], ['Idempotent-Replay: true']];
        }
        foreach ($s['orders'] as $stored) {
            if ($stored['orderId'] === $order['orderId']) {
                return [409, ['error' => 'order already registered under another key'], []];
            }
        }
        $s['seq']++;
        $response = ['erpOrderId' => 'ERP-' . $s['seq'], 'status' => 'registered'];
        $s['orders'][$key] = ['orderId' => $order['orderId'], 'response' => $response];
        return [201, $response, []];
    });
    respond($status, $body, $headers);
}

function handleConfig(string $method): never
{
    if ($method === 'GET') {
        respond(200, withState(fn(array &$s) => $s['config']));
    }
    $in = jsonBody();
    $rate = $in['errorRate'] ?? null;
    $latency = $in['latencyMs'] ?? null;
    if (!is_numeric($rate) || $rate < 0 || $rate > 1 || !is_int($latency) || $latency < 0 || $latency > 10000) {
        respond(400, ['error' => 'errorRate must be between 0 and 1 and latencyMs an integer between 0 and 10000']);
    }
    $config = ['errorRate' => $rate + 0, 'latencyMs' => $latency];
    withState(function (array &$s) use ($config) {
        $s['config'] = $config;
    });
    respond(200, $config);
}

$method = $_SERVER['REQUEST_METHOD'];
$path = parse_url($_SERVER['REQUEST_URI'], PHP_URL_PATH);

match (true) {
    $method === 'GET' && $path === '/healthz' => respond(200, ['status' => 'ok']),
    $method === 'POST' && $path === '/orders' => handleOrder(),
    $path === '/admin/config' && in_array($method, ['GET', 'PUT'], true) => handleConfig($method),
    $method === 'GET' && $path === '/admin/orders' => respond(200, withState(fn(array &$s) => [
        'count' => count($s['orders']),
        'orders' => array_values(array_map(fn($o) => $o['orderId'], $s['orders'])),
    ])),
    $method === 'POST' && $path === '/admin/reset' => (function () {
        withState(function (array &$s) {
            $s = defaultState();
        });
        respond(200, ['reset' => true]);
    })(),
    default => respond(404, ['error' => 'not found']),
};
