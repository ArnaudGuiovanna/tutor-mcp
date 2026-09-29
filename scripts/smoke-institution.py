#!/usr/bin/env python3
"""Institution journey over the real HTTP boundary (driven by smoke-institution.sh).

A pedagogy manager and a learner of a provisioned tenant sign in through
/authorize with PKCE, exchange and refresh their tokens, the manager publishes
a formation and enrolls the learner through the admin API, and the learner
uses the MCP tutor. Standard library only.
"""
import base64
import hashlib
import http.client
import json
import os
import re
import secrets
import sys
import time
from urllib.parse import urlencode, urlsplit, parse_qs

BASE = os.environ['SMOKE_BASE_URL']
HOST, PORT = os.environ['SMOKE_CONNECT'].split(':')
TENANT = os.environ['SMOKE_TENANT']
PASSWORD = os.environ['SMOKE_PASSWORD']
IAT = os.environ['OAUTH_DCR_INITIAL_ACCESS_TOKEN']
REDIRECT = 'http://127.0.0.1:19090/callback'


class Session:
    def __init__(self):
        self.cookie = ''

    def request(self, method, path, body=None, content_type=None, bearer=None, headers=None, attempt=0):
        conn = http.client.HTTPConnection(HOST, int(PORT), timeout=60)
        h = {'Host': urlsplit(BASE).netloc, 'Accept': 'application/json, text/event-stream'}
        if self.cookie:
            h['Cookie'] = self.cookie
        if content_type:
            h['Content-Type'] = content_type
        if bearer:
            h['Authorization'] = 'Bearer ' + bearer
        h.update(headers or {})
        conn.request(method, path, body, h)
        response = conn.getresponse()
        status, location, retry = response.status, response.getheader('Location'), response.getheader('Retry-After')
        cookie = response.getheader('Set-Cookie')
        if cookie:
            self.cookie = cookie.split(';', 1)[0]
        data = response.read().decode()
        conn.close()
        if status == 429 and attempt < 8:
            time.sleep(min(30, max(1, int(retry or '5'))))
            return self.request(method, path, body, content_type, bearer, headers, attempt + 1)
        return status, location, data

    def form(self, path, values):
        return self.request('POST', path, urlencode(values), 'application/x-www-form-urlencoded')


def parse(body):
    if body.startswith('event:') or body.startswith('data:'):
        body = next(line[5:].strip() for line in body.splitlines() if line.startswith('data:'))
    return json.loads(body)


def check(condition, message):
    if not condition:
        print('FAIL: ' + message, file=sys.stderr)
        sys.exit(1)


def claims(token):
    payload = token.split('.')[1]
    return json.loads(base64.urlsafe_b64decode(payload + '=' * (-len(payload) % 4)))


def sign_in(email, client_id):
    session = Session()
    verifier = secrets.token_urlsafe(48)
    params = {'response_type': 'code', 'client_id': client_id, 'redirect_uri': REDIRECT, 'scope': 'learner',
              'resource': BASE + '/mcp', 'state': 'smoke', 'code_challenge_method': 'S256',
              'code_challenge': base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip('=')}
    status, _, page = session.request('GET', '/authorize?' + urlencode(params))
    check(status == 200, f'{email}: authorize page {status}')
    params.update(csrf_token=re.search(r'name="csrf_token"\s+value="([^"]+)"', page).group(1),
                  mode='login', email=email, password=PASSWORD, approve_client='yes')
    status, location, _ = session.form('/authorize', params)
    check(status == 302, f'{email}: login {status}')
    code = parse_qs(urlsplit(location).query)['code'][0]
    status, _, body = session.form('/token', {'grant_type': 'authorization_code', 'code': code, 'client_id': client_id,
                                              'redirect_uri': REDIRECT, 'resource': BASE + '/mcp', 'code_verifier': verifier})
    check(status == 200, f'{email}: token exchange {status} {body[:120]}')
    tokens = json.loads(body)
    check(claims(tokens['access_token']).get('tid') == TENANT, f'{email}: token not bound to the institution')
    status, _, body = session.form('/token', {'grant_type': 'refresh_token', 'refresh_token': tokens['refresh_token'],
                                              'client_id': client_id, 'resource': BASE + '/mcp'})
    check(status == 200, f'{email}: refresh {status} {body[:120]}')
    return json.loads(body)['access_token']


def admin(token, method, path, payload=None, key=None):
    headers = {'Idempotency-Key': key} if key else {}
    status, _, body = Session().request(method, path, json.dumps(payload) if payload is not None else None,
                                        'application/json' if payload is not None else None, token, headers)
    return status, (json.loads(body) if body.strip().startswith(('{', '[')) else body)


def main():
    status, _, body = Session().request('POST', '/register', json.dumps({
        'client_name': 'Institution smoke connector', 'redirect_uris': [REDIRECT],
        'token_endpoint_auth_method': 'none'}), 'application/json', headers={'Authorization': 'Bearer ' + IAT})
    check(status == 201, f'DCR registration {status}')
    client_id = json.loads(body)['client_id']

    manager = sign_in('manager@acme.test', client_id)
    learner = sign_in('alice@acme.test', client_id)
    print('PASS: institution members sign in, exchange and refresh tokens bound to their tenant')

    status, formation = admin(manager, 'POST', '/admin/catalog/formations', {'name': 'Go backend', 'description': 'Smoke formation'}, 'f1')
    check(status == 201, f'create formation {status}')
    version = formation['version']['ID']
    status, _ = admin(manager, 'POST', f'/admin/catalog/formation-versions/{version}/modules',
                      {'StableKey': 'm1', 'Title': 'Foundations', 'Position': 0}, 'm1')
    check(status == 201, f'create module {status}')
    for position, (key, label, prerequisites) in enumerate([('variables', 'Variables', []), ('functions', 'Functions', ['variables'])]):
        status, _ = admin(manager, 'POST', f'/admin/catalog/formation-versions/{version}/concepts',
                          {'ModuleStableKey': 'm1', 'StableKey': key, 'Label': label, 'Position': position,
                           'Prerequisites': prerequisites}, 'c-' + key)
        check(status == 201, f'create concept {key} {status}')
    status, _ = admin(manager, 'POST', f'/admin/catalog/formation-versions/{version}/publish', None, 'publish')
    check(status == 200, f'publish {status}')
    status, cohort = admin(manager, 'POST', '/admin/catalog/cohorts', {'formation_version_id': version, 'name': 'Smoke cohort', 'capacity': 10}, 'cohort')
    check(status == 201, f'create cohort {status}')
    status, _ = admin(manager, 'POST', f"/admin/catalog/cohorts/{cohort['cohort']['ID']}/enrollments",
                      {'membership_id': 'mem_alice', 'objectives': {}}, 'enroll')
    check(status == 201, f'enroll learner {status}')
    print('PASS: pedagogy manager publishes a formation and enrolls the learner')

    status, body = admin(learner, 'POST', '/admin/catalog/formations', {'name': 'x', 'description': 'x'}, 'forbidden')
    check(status == 403, f'learner reached the admin API: {status}')

    session, number = Session(), 0

    def mcp(method, params):
        nonlocal number
        number += 1
        status, _, body = session.request('POST', '/mcp', json.dumps({'jsonrpc': '2.0', 'id': number, 'method': method, 'params': params}),
                                          'application/json', learner)
        check(status == 200, f'MCP {method} {status}')
        result = parse(body)
        check('error' not in result, f'MCP {method} error {result.get("error")}')
        return result['result']

    mcp('initialize', {'protocolVersion': '2025-11-25', 'capabilities': {}, 'clientInfo': {'name': 'smoke', 'version': '1'}})
    check(len(mcp('tools/list', {})['tools']) > 0, 'no MCP tools')
    result = mcp('tools/call', {'name': 'init_domain', 'arguments': {'name': 'Smoke domain', 'concepts': ['fractions'],
                                                                     'prerequisites': {}, 'idempotency_key': 'smoke-domain'}})
    check(not result.get('isError'), 'init_domain failed')
    result = mcp('tools/call', {'name': 'get_next_activity', 'arguments': {}})
    check(not result.get('isError'), 'get_next_activity failed')
    print('PASS: institution learner uses the MCP tutor')


if __name__ == '__main__':
    main()
