#!/usr/bin/env python3
"""Exercise an isolated hobby server through HTTP and its real OAuth/MCP boundary.

Use only with disposable test accounts. The optional invitation file is the
captured output of `tutor-mcp init` or `users invite`. Re-run without it after
restarting the server to check persisted credentials and domain idempotency.
"""
import argparse
import base64
import hashlib
import http.client
import json
from pathlib import Path
import re
import socket
import ssl
import time
from urllib.parse import urlencode, urlsplit, parse_qs


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--base-url', required=True)
    parser.add_argument('--connect-url', required=True)
    parser.add_argument('--invite-file')
    parser.add_argument('--reset-file')
    parser.add_argument('--client-id-file', help='reuse a registered public client across acceptance steps')
    parser.add_argument('--username', default='acceptance-user')
    parser.add_argument('--password', default='Acceptance-only-password-123')
    parser.add_argument('--expect-persistence', action='store_true')
    parser.add_argument('--expect-login-denied', action='store_true')
    parser.add_argument('--insecure-local-tls', action='store_true')
    args = parser.parse_args()
    origin, connect = urlsplit(args.base_url), urlsplit(args.connect_url)
    password = args.password
    cookie = ''
    tls = ssl.create_default_context()
    if args.insecure_local_tls:
        if connect.hostname not in ('localhost', '127.0.0.1', '::1'):
            parser.error('insecure test TLS is restricted to loopback')
        tls.check_hostname = False
        tls.verify_mode = ssl.CERT_NONE

    class RoutedHTTPSConnection(http.client.HTTPSConnection):
        def connect(self):
            raw = socket.create_connection((connect.hostname, connect.port or 443), self.timeout)
            self.sock = tls.wrap_socket(raw, server_hostname=origin.hostname)

    def request(method, path, body=None, content_type=None, bearer=None, attempt=0):
        nonlocal cookie
        if connect.scheme == 'https':
            conn = RoutedHTTPSConnection(origin.hostname, origin.port, context=tls, timeout=30)
        else:
            conn = http.client.HTTPConnection(connect.hostname, connect.port, timeout=30)
        headers = {'Host': origin.netloc, 'Accept': 'application/json, text/event-stream',
                   'X-Forwarded-Proto': 'https'}
        if cookie:
            headers['Cookie'] = cookie
        if content_type:
            headers['Content-Type'] = content_type
        if bearer:
            headers['Authorization'] = 'Bearer ' + bearer
        conn.request(method, path, body, headers)
        response = conn.getresponse()
        status, location = response.status, response.getheader('Location')
        retry_after = response.getheader('Retry-After')
        set_cookie = response.getheader('Set-Cookie')
        if set_cookie:
            cookie = set_cookie.split(';', 1)[0]
        data = response.read().decode()
        conn.close()
        if status == 429 and attempt < 6:
            # Keep the production limits enabled during acceptance. A burst of
            # operator scenarios from one IP may need more than one refill.
            delay = min(30, max(1, int(retry_after or '5')))
            print(f'Waiting {delay}s for the server rate limit', flush=True)
            time.sleep(delay)
            return request(method, path, body, content_type, bearer, attempt + 1)
        return status, location, data

    def form(path, values):
        return request('POST', path, urlencode(values), 'application/x-www-form-urlencoded')

    def csrf(page):
        return re.search(r'name="csrf_token"\s+value="([^"]+)"', page).group(1)

    def json_response(result, expected=200):
        status, _, body = result
        if status not in (expected if isinstance(expected, tuple) else (expected,)):
            raise RuntimeError(f'Unexpected HTTP status {status}, expected {expected}')
        if body.startswith('event:') or body.startswith('data:'):
            body = next(line[5:].strip() for line in body.splitlines() if line.startswith('data:'))
        return json.loads(body)

    assert request('GET', '/ready')[0] == 200
    discovery = json_response(request('GET', '/.well-known/oauth-authorization-server'))
    assert discovery['authorization_endpoint'] == args.base_url + '/authorize'
    protected = json_response(request('GET', '/.well-known/oauth-protected-resource'))
    assert protected['resource'] == args.base_url + '/mcp'
    if args.invite_file or args.reset_file:
        purpose = 'invite' if args.invite_file else 'reset'
        link = re.search(r'https://\S+/account/' + purpose + r'\?token=\S+',
                         Path(args.invite_file or args.reset_file).read_text()).group(0)
        account_link = urlsplit(link)
        status, _, page = request('GET', account_link.path + '?' + account_link.query)
        assert status == 200
        values = {'csrf_token': csrf(page), 'token': parse_qs(account_link.query)['token'][0],
                  'login_name': args.username, 'password': password, 'password_confirm': password}
        assert form(account_link.path, values)[0] == 200
        assert request('GET', account_link.path + '?' + account_link.query)[0] == 400
    client_file = Path(args.client_id_file) if args.client_id_file else None
    if client_file and client_file.exists():
        registration = {'client_id': client_file.read_text().strip()}
    else:
        registration = json_response(request('POST', '/register', json.dumps({
            'client_name': 'Disposable acceptance fixture',
            'redirect_uris': ['http://127.0.0.1:19090/callback'],
            'token_endpoint_auth_method': 'none'}), 'application/json'), (200, 201))
        if client_file:
            client_file.write_text(registration['client_id'])
    verifier = 'v' * 43
    params = {'response_type': 'code', 'client_id': registration['client_id'],
              'redirect_uri': 'http://127.0.0.1:19090/callback', 'scope': 'learner',
              'resource': args.base_url + '/mcp', 'state': 'acceptance-state',
              'code_challenge_method': 'S256',
              'code_challenge': base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip('=')}
    status, _, page = request('GET', '/authorize?' + urlencode(params))
    assert status == 200
    params.update(csrf_token=csrf(page), mode='login', email=args.username, password=password, approve_client='yes')
    status, location, _ = form('/authorize', params)
    if args.expect_login_denied:
        assert status == 401 and location is None, f'Expected credential rejection, got {status}'
        print('PASS: rejected invalid or disabled credentials')
        return
    assert status == 302, f'Login/consent failed: {status}'
    query = parse_qs(urlsplit(location).query)
    assert query['state'] == ['acceptance-state']
    tokens = json_response(form('/token', {'grant_type': 'authorization_code', 'code': query['code'][0],
        'client_id': registration['client_id'], 'redirect_uri': params['redirect_uri'],
        'resource': params['resource'], 'code_verifier': verifier}))

    def mcp(method, params, number):
        result = json_response(request('POST', '/mcp', json.dumps({
            'jsonrpc': '2.0', 'id': number, 'method': method, 'params': params}),
            'application/json', tokens['access_token']))
        assert 'error' not in result, f'MCP {method} failed'
        return result['result']

    mcp('initialize', {'protocolVersion': '2025-11-25', 'capabilities': {},
                      'clientInfo': {'name': 'acceptance', 'version': '1'}}, 1)
    assert mcp('tools/list', {}, 2)['tools']
    result = mcp('tools/call', {'name': 'init_domain', 'arguments': {
        'name': 'Deployment persistence', 'concepts': ['fractions'], 'prerequisites': {},
        'idempotency_key': 'deployment-persistence'}}, 3)
    assert not result.get('isError'), 'Domain mutation failed'
    if args.invite_file:
        assert not result['structuredContent'].get('idempotent_replay'), 'New account reused another learner mutation'
    if args.expect_persistence:
        assert result['structuredContent']['idempotent_replay'] is True, 'Domain did not survive restart'
    refreshed = json_response(form('/token', {'grant_type': 'refresh_token',
        'refresh_token': tokens['refresh_token'], 'client_id': registration['client_id'],
        'resource': params['resource']}))
    assert refreshed['refresh_token'] != tokens['refresh_token']
    print('PASS: discovery, account forms/login, consent, PKCE, MCP tools, domain isolation/persistence and refresh rotation')


if __name__ == '__main__':
    main()
