#!/usr/bin/env python3
"""Institution journeys over the real HTTP boundary (driven by smoke-institution.sh).

1. The owner of an operator-provisioned institution accepts the owner
   invitation, enrolls TOTP and invites a staff-only pedagogy manager and two
   learners from the console. They accept by email link; the manager enrolls TOTP.
2. Members sign in through /authorize, exchange and refresh tenant-bound tokens.
   The manager authors and publishes through MCP; learners enroll through MCP
   and /learn. Their tutor domains share concept IDs. Staff progression uses a
   separate OAuth grant and live cohort assignments. Clone, migrate and archive
   exercise the formation lifecycle and historical report isolation.
3. A second institution is created through self-service signup.

Standard library only.
"""
import base64
import email
import glob
import hashlib
import hmac
import html
import http.client
import json
import os
import re
import secrets
import struct
import sys
import time
from urllib.parse import urlencode, urlsplit, parse_qs

BASE = os.environ['SMOKE_BASE_URL']
HOST, PORT = os.environ['SMOKE_CONNECT'].split(':')
TENANT = os.environ['SMOKE_TENANT']
OWNER_LINK = os.environ['SMOKE_OWNER_LINK']
MAIL_DIR = os.environ['SMOKE_MAIL_DIR']
IAT = os.environ['OAUTH_DCR_INITIAL_ACCESS_TOKEN']
REDIRECT = 'http://127.0.0.1:19090/callback'


def check(condition, message):
    if not condition:
        print('FAIL: ' + message, file=sys.stderr)
        sys.exit(1)


class Browser:
    """A minimal cookie-keeping client that follows same-site redirects."""

    def __init__(self):
        self.cookies = {}

    def request(self, method, path, body=None, content_type=None, bearer=None, headers=None, attempt=0):
        conn = http.client.HTTPConnection(HOST, int(PORT), timeout=60)
        h = {'Host': urlsplit(BASE).netloc, 'Accept': 'application/json, text/event-stream, text/html'}
        if self.cookies:
            h['Cookie'] = '; '.join(f'{k}={v}' for k, v in self.cookies.items())
        if content_type:
            h['Content-Type'] = content_type
        if bearer:
            h['Authorization'] = 'Bearer ' + bearer
        h.update(headers or {})
        conn.request(method, path, body, h)
        response = conn.getresponse()
        status, location, retry = response.status, response.getheader('Location'), response.getheader('Retry-After')
        for header, value in response.getheaders():
            if header.lower() != 'set-cookie':
                continue
            name, _, rest = value.partition('=')
            cookie_value = rest.split(';', 1)[0]
            if 'max-age=0' in value.lower() or cookie_value == '':
                self.cookies.pop(name, None)
            else:
                self.cookies[name] = cookie_value
        data = response.read().decode()
        conn.close()
        if status == 429 and attempt < 8:
            time.sleep(min(30, max(1, int(retry or '5'))))
            return self.request(method, path, body, content_type, bearer, headers, attempt + 1)
        return status, location, data

    def get(self, path):
        status, location, body = self.request('GET', path)
        while status in (302, 303) and location and location.startswith('/'):
            path = location
            status, location, body = self.request('GET', path)
        return status, path, body

    def post(self, path, values):
        status, location, body = self.request('POST', path, urlencode(values, doseq=True), 'application/x-www-form-urlencoded')
        if status in (302, 303) and location and location.startswith('/'):
            return self.get(location)
        return status, path, body


def local_path(link):
    parts = urlsplit(link)
    return parts.path + ('?' + parts.query if parts.query else '')


def field(pattern, page, what):
    match = re.search(pattern, page)
    check(match is not None, f'{what} not found in page')
    return html.unescape(match.group(1))


def csrf(page):
    return field(r'name="csrf_token"\s+value="([^"]+)"', page, 'CSRF token')


def totp(secret, at=None):
    key = base64.b32decode(secret + '=' * (-len(secret) % 8))
    counter = int((at or time.time()) // 30)
    digest = hmac.new(key, struct.pack('>Q', counter), hashlib.sha1).digest()
    offset = digest[-1] & 0x0f
    value = struct.unpack('>I', digest[offset:offset + 4])[0] & 0x7fffffff
    return f'{value % 1_000_000:06d}'


def fresh_totp(secret, used):
    """A code from a time step not used before (the server rejects replays)."""
    while True:
        step = int(time.time() // 30)
        if step not in used:
            used.add(step)
            return totp(secret)
        time.sleep(1)


def mail_link(path, seen):
    """The link of the oldest unread message pointing to path (sent in order)."""
    deadline = time.time() + 30
    while time.time() < deadline:
        for name in sorted(glob.glob(os.path.join(MAIL_DIR, '*.eml'))):
            if name in seen:
                continue
            with open(name) as handle:
                body = email.message_from_string(handle.read()).get_payload(decode=True).decode()
            match = re.search(re.escape(BASE + path) + r'\?token=[A-Za-z0-9_%-]+', body)
            if match:
                seen.add(name)
                return match.group(0)
        time.sleep(0.5)
    check(False, f'no email with a {path} link')


def enroll_totp(browser, page):
    secret = field(r'<span class="mono">([A-Z2-7]{32})</span>', page, 'TOTP secret')
    status, _, body = browser.post('/console/mfa/setup', {
        'csrf_token': csrf(page), 'credential_id': field(r'name="credential_id" value="([^"]+)"', page, 'credential'),
        'code': totp(secret)})
    check(status == 200 and 'Save your recovery codes' in body, f'TOTP enrollment {status}')
    return secret


def accept_invitation(link, password):
    browser = Browser()
    status, _, page = browser.get(local_path(link))
    check(status == 200, f'invitation page {status}')
    status, path, page = browser.post('/invite', {
        'csrf_token': csrf(page), 'token': field(r'name="token" value="([^"]+)"', page, 'invitation token'),
        'password': password, 'password_confirm': password})
    check(status == 200, f'invitation acceptance {status}')
    return browser, path, page


def claims(token):
    payload = token.split('.')[1]
    return json.loads(base64.urlsafe_b64decode(payload + '=' * (-len(payload) % 4)))


def sign_in(email_address, password, client_id, code=None, scope="learner"):
    browser = Browser()
    verifier = secrets.token_urlsafe(48)
    params = {'response_type': 'code', 'client_id': client_id, 'redirect_uri': REDIRECT, 'scope': scope,
              'resource': BASE + '/mcp', 'state': 'smoke', 'code_challenge_method': 'S256',
              'code_challenge': base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip('=')}
    status, _, page = browser.request('GET', '/authorize?' + urlencode(params))
    check(status == 200, f'{email_address}: authorize page {status}')
    form = dict(params, csrf_token=csrf(page), mode='login', email=email_address, password=password, approve_client='yes')
    if code:
        form['totp_code'] = code
    status, location, body = browser.request('POST', '/authorize', urlencode(form), 'application/x-www-form-urlencoded')
    check(status == 302, f'{email_address}: login {status} {re.sub(r"<[^>]+>", " ", body)[-300:]}')
    code_value = parse_qs(urlsplit(location).query)['code'][0]
    status, _, body = browser.request('POST', '/token', urlencode({
        'grant_type': 'authorization_code', 'code': code_value, 'client_id': client_id,
        'redirect_uri': REDIRECT, 'resource': BASE + '/mcp', 'code_verifier': verifier}),
        'application/x-www-form-urlencoded')
    check(status == 200, f'{email_address}: token exchange {status} {body[:120]}')
    tokens = json.loads(body)
    check(claims(tokens['access_token']).get('tid') == TENANT, f'{email_address}: token not bound to the institution')
    status, _, body = browser.request('POST', '/token', urlencode({
        'grant_type': 'refresh_token', 'refresh_token': tokens['refresh_token'],
        'client_id': client_id, 'resource': BASE + '/mcp'}), 'application/x-www-form-urlencoded')
    check(status == 200, f'{email_address}: refresh {status} {body[:120]}')
    return json.loads(body)['access_token']


def admin(token, method, path, payload=None, key=None):
    headers = {'Idempotency-Key': key} if key else {}
    status, _, body = Browser().request(method, path, json.dumps(payload) if payload is not None else None,
                                        'application/json' if payload is not None else None, token, headers)
    return status, (json.loads(body) if body.strip().startswith(('{', '[')) else body)


def parse(body):
    if body.startswith('event:') or body.startswith('data:'):
        body = next(line[5:].strip() for line in body.splitlines() if line.startswith('data:'))
    return json.loads(body)


def main():
    seen_mail = set()
    used_steps = {}

    # 1. Owner bootstrap from the operator's invitation link, then invitations.
    owner, path, page = accept_invitation(OWNER_LINK, 'owner-password-2026')
    check(path == '/console/mfa/setup', f'owner landed on {path}')
    owner_secret = enroll_totp(owner, page)
    used_steps[owner_secret] = {int(time.time() // 30)}
    status, _, page = owner.get('/console')
    check(status == 200 and 'owner@acme.test (you)' in page, f'owner console {status}')
    status, _, page = owner.post('/console/members/invite', {
        'csrf_token': csrf(page), 'emails': 'manager@acme.test', 'roles': ['pedagogy_manager']})
    check(status == 200 and 'invitation(s) created' in page, f'invite manager {status}')
    status, _, page = owner.post('/console/members/invite', {
        'csrf_token': csrf(page), 'emails': 'alice@acme.test', 'roles': ['learner']})
    check(status == 200, f'invite learner {status}')
    manager_link = mail_link('/invite', seen_mail)
    alice_link = mail_link('/invite', seen_mail)

    manager, path, page = accept_invitation(manager_link, 'manager-password-2026')
    check(path == '/console/mfa/setup', f'manager landed on {path}')
    manager_secret = enroll_totp(manager, page)
    used_steps[manager_secret] = {int(time.time() // 30)}
    _, path, page = accept_invitation(alice_link, 'alice-password-2026')
    check('/mcp' in page, 'learner was not sent to her AI client')
    status, _, page = owner.get('/console')
    status, _, page = owner.post('/console/members/invite', {
        'csrf_token': csrf(page), 'emails': 'bob@acme.test', 'roles': ['learner']})
    check(status == 200, f'invite Bob {status}')
    _, _, page = accept_invitation(mail_link('/invite', seen_mail), 'bob-password-2026')
    check('/mcp' in page, 'Bob invitation did not create a learner')
    status, _, page = owner.get('/console')
    status, _, page = owner.post('/console/members/invite', {
        'csrf_token': csrf(page), 'emails': 'trainer@acme.test', 'roles': ['trainer']})
    check(status == 200, f'invite trainer {status}')
    trainer_browser, path, page = accept_invitation(mail_link('/invite', seen_mail), 'trainer-password-2026')
    check(path == '/console/mfa/setup', 'trainer skipped MFA setup')
    trainer_secret = enroll_totp(trainer_browser, page)
    used_steps[trainer_secret] = {int(time.time() // 30)}
    print('PASS: owner invitation, console TOTP enrollment and member invitations by email')

    # 2. AI-client sign-in with tenant-bound tokens.
    status, _, body = Browser().request('POST', '/register', json.dumps({
        'client_name': 'Institution smoke connector', 'redirect_uris': [REDIRECT],
        'token_endpoint_auth_method': 'none'}), 'application/json', headers={'Authorization': 'Bearer ' + IAT})
    check(status == 201, f'DCR registration {status}')
    client_id = json.loads(body)['client_id']
    manager_token = sign_in('manager@acme.test', 'manager-password-2026', client_id,
                            fresh_totp(manager_secret, used_steps[manager_secret]), "formation:read formation:write progress:read")
    trainer_token = sign_in('trainer@acme.test', 'trainer-password-2026', client_id,
                           fresh_totp(trainer_secret, used_steps[trainer_secret]), 'progress:read')
    check(not claims(trainer_token).get('learner_id'), 'trainer received a learner identity')
    learner_token = sign_in('alice@acme.test', 'alice-password-2026', client_id)
    print('PASS: members sign in (manager with TOTP), exchange and refresh tokens bound to their tenant')

    check(not claims(manager_token).get('learner_id'), 'staff OAuth unexpectedly requires a learner profile')
    bob_token = sign_in('bob@acme.test', 'bob-password-2026', client_id)
    for token in [manager_token, learner_token]:
        status, _ = admin(token, 'POST', '/admin/catalog/formations', {'name': 'forbidden'}, 'old-admin')
        check(status == 403, f'MCP bearer reached administrative API: {status}')
    status, _, _ = Browser().request('GET', '/console/admin/catalog/formations', bearer=manager_token)
    check(status == 303, f'MCP bearer opened the console API: {status}')
    for token, forbidden_tool, arguments in [
        (learner_token, 'draft_formation', {'name': 'Forbidden', 'idempotency_key': 'no-authority'}),
        (manager_token, 'get_learner_context', {})]:
        status, _, body = Browser().request('POST', '/mcp', json.dumps({
            'jsonrpc': '2.0', 'id': 1, 'method': 'tools/call',
            'params': {'name': forbidden_tool, 'arguments': arguments}}), 'application/json', token)
        check(status == 403, f'wrong role/scope reached {forbidden_tool}: {status} {body[:160]}')

    def console_api(browser, method, path, payload=None, key=None):
        headers = {'Idempotency-Key': key} if key else {}
        if method not in ('GET', 'HEAD'):
            status, _, body = browser.get('/console/api-csrf')
            check(status == 200, f'console CSRF {status}')
            headers['X-CSRF-Token'] = json.loads(body)['csrf_token']
        status, _, body = browser.request(method, '/console' + path,
            json.dumps(payload) if payload is not None else None, 'application/json', headers=headers)
        return status, (json.loads(body) if body.strip().startswith(('{', '[')) else body)

    def mcp_client(token):
        browser, number = Browser(), 0
        def call(method, params):
            nonlocal number
            number += 1
            status, _, body = browser.request('POST', '/mcp', json.dumps({
                'jsonrpc': '2.0', 'id': number, 'method': method, 'params': params}), 'application/json', token)
            check(status == 200, f'MCP {method} {status}: {body[:300]}')
            result = parse(body)
            check('error' not in result, f'MCP {method}: {result.get("error")}')
            return result['result']
        call('initialize', {'protocolVersion': '2025-11-25', 'capabilities': {}, 'clientInfo': {'name': 'smoke', 'version': '2'}})
        return call

    def tool(client, name, arguments):
        result = client('tools/call', {'name': name, 'arguments': arguments})
        check(not result.get('isError'), f'{name}: {result}')
        return result.get('structuredContent') or json.loads(result['content'][0]['text'])

    author = mcp_client(manager_token)
    draft = tool(author, 'draft_formation', {'name': 'Go backend', 'description': 'Shared smoke formation', 'idempotency_key': 'f1'})
    formation_id, version = draft['formation']['id'], draft['version']['id']
    content = tool(author, 'add_formation_concepts', {'version_id': version, 'idempotency_key': 'content', 'concepts': [
        {'stable_key': 'variables', 'label': 'Variables', 'position': 0, 'description': 'Read a variable'},
        {'stable_key': 'functions', 'label': 'Functions', 'position': 1, 'prerequisites': ['variables'], 'description': 'Call a function'}]})
    shared_ids = {c['stable_key']: c['id'] for c in content['concepts']}
    review = tool(author, 'get_formation_version', {'version_id': version})
    check(review['concepts'][0]['description'] == 'Read a variable', 'pedagogical metadata lost')
    tool(author, 'publish_formation', {'version_id': version, 'idempotency_key': 'publish'})
    status, cohort = console_api(manager, 'POST', '/admin/catalog/cohorts',
        {'formation_version_id': version, 'name': 'Smoke cohort', 'capacity': 10}, 'cohort')
    check(status == 201, f'create cohort {status}: {cohort}')
    cohort_id = cohort['cohort']['id']
    bob_browser = Browser()
    _, _, page = bob_browser.get('/learn/login')
    status, path, page = bob_browser.post('/learn/login', {
        'csrf_token': csrf(page), 'email': 'bob@acme.test', 'password': 'bob-password-2026'})
    check(status == 200 and path == '/learn' and 'Smoke cohort' in page, 'learner portal login/catalog')
    enrollments = []
    for name, token in [('alice', learner_token), ('bob', bob_token)]:
        learner = mcp_client(token)
        catalog = tool(learner, 'list_available_formations', {})
        check(any(f['cohort_id'] == cohort_id for f in catalog['items']), 'MCP catalog missing cohort')
        check(not any(f['name'] == 'Legacy recovery' for f in catalog['items']), 'private recovery data leaked to catalog')
        denied = learner('tools/call', {'name': 'join_formation', 'arguments': {
            'cohort_id': cohort_id, 'idempotency_key': 'uninvited-' + name}})
        check(denied.get('isError'), 'invitation policy bypassed')
        status, admission = console_api(manager, 'POST', f'/admin/catalog/cohorts/{cohort_id}/admissions',
            {'membership_id': claims(token)['membership_id'], 'decision': 'invited', 'expected_version': 0}, 'invite-' + name)
        check(status == 201, f'formation invitation {status}: {admission}')
        if name == 'alice':
            enrollment = tool(learner, 'join_formation', {'cohort_id': cohort_id, 'idempotency_key': 'self-join-alice'})['enrollment']
        else:
            _, _, page = bob_browser.get('/learn')
            status, _, page = bob_browser.post('/learn/enrollment', {
                'csrf_token': csrf(page), 'cohort_id': cohort_id, 'action': 'join', 'idempotency_key': 'web-join-bob'})
            check(status == 200 and 'Leave and keep my progress' in page, f'web enrollment {status}')
            own = tool(learner, 'get_my_formations', {})
            enrollment = next(f['enrollment'] for f in own['items'] if f['cohort_id'] == cohort_id)
        check(bool(enrollment.get('domain_id')), 'enrollment did not create a tutor domain')
        enrollments.append(enrollment)
        snapshot = tool(learner, 'get_curriculum_snapshot', {'domain_id': enrollment['domain_id']})
        # The tutor returns an immutable curriculum snapshot.
        curriculum = snapshot.get('curriculum') or snapshot.get('snapshot') or snapshot
        check({c['key']: c['formation_concept_id'] for c in curriculum['concepts']} == shared_ids,
              'learners do not share the published concept IDs')
        denied = learner('tools/call', {'name': 'add_concepts', 'arguments': {
            'domain_id': enrollment['domain_id'], 'concepts': ['invented'], 'prerequisites': {}, 'expected_version': 1}})
        check(denied.get('isError'), 'learner rewrote the formation curriculum')
        activity = tool(learner, 'get_next_activity', {'domain_id': enrollment['domain_id']})
        check(activity['domain_id'] == enrollment['domain_id'], 'tutor selected an unrelated domain')
        prepared = tool(learner, 'prepare_assessment_attempt', {
            'domain_id': enrollment['domain_id'], 'concept': 'variables', 'activity_type': 'PRACTICE',
            'observable': 'Read x', 'task_text': 'Given x = 3, what is x?',
            'rubric_json': json.dumps({'criteria': [{'id': 'correct', 'description': 'Answer is 3', 'max_score': 1}], 'passing_score': 1})})
        tool(learner, 'submit_assessment_attempt', {'attempt_id': prepared['attempt_id'], 'learner_response': '3'})
        tool(learner, 'record_interaction', {'domain_id': enrollment['domain_id'], 'concept': 'variables',
            'activity_type': 'PRACTICE', 'success': True, 'response_time_seconds': 5, 'confidence': 0.9,
            'attempt_id': prepared['attempt_id'], 'evaluator_id': 'smoke-host', 'evaluation_method': 'host_llm',
            'rubric_score_json': json.dumps({'criteria_scores': [{'id': 'correct', 'score': 1}], 'total': 1, 'max_total': 1})})
    check(enrollments[0]['domain_id'] != enrollments[1]['domain_id'], 'learners share a private domain')
    status, report = console_api(manager, 'GET', f'/admin/catalog/cohorts/{cohort_id}/report')
    check(status == 200 and report['enrollment_count'] == 2 and report['active_count'] == 2,
          f'cohort report counts concepts instead of learners: {report}')
    check(report['average_mastery'] > 0.1001, f'answers did not advance cohort mastery beyond its initial value: {report}')
    print('PASS: staff-only MCP authoring, console-only administration, shared concepts and two learners tutoring')

    # Phase 5: ordinary practice must not mint mastery/completion/retention badges.
    alice = mcp_client(learner_token)
    check(tool(alice, 'get_my_badges', {'enrollment_id': enrollments[0]['id']})['items'] == [], 'practice minted a badge')
    denied = alice('tools/call', {'name': 'get_my_badges', 'arguments': {'enrollment_id': enrollments[1]['id']}})
    check(denied.get('isError') and 'not_found' in json.dumps(denied), 'other learner badge disclosure')
    status, _, page = bob_browser.get('/learn/badges?enrollment_id=' + enrollments[1]['id'])
    check(status == 200 and 'No badges earned' in page, 'learner badge browser read')

    # Phase 4: staff OAuth, named follow-up, browser dashboard and live revocation.
    trainer = mcp_client(trainer_token)
    check(tool(trainer, 'list_trainer_cohorts', {})['items'] == [], 'unassigned trainer listed cohorts')
    denied = trainer('tools/call', {'name': 'get_cohort_insights', 'arguments': {'cohort_id': cohort_id}})
    check(denied.get('isError'), 'unassigned trainer read cohort progress')
    status, _, page = manager.get('/console/progress?cohort_id=' + cohort_id)
    check(status == 200 and 'alice@acme.test' in page and 'bob@acme.test' in page, 'manager dashboard missing learners')
    status, _, page = manager.post('/console/progress/trainers', {
        'csrf_token': csrf(page), 'cohort_id': cohort_id, 'email': 'trainer@acme.test', 'assigned': 'true'})
    check(status == 200 and 'trainer@acme.test' in page, 'trainer assignment failed')
    accessible = tool(trainer, 'list_trainer_cohorts', {})
    check(len(accessible['items']) == 1 and accessible['items'][0]['cohort_id'] == cohort_id, 'trainer assignment scope')
    insights = tool(trainer, 'get_cohort_insights', {'cohort_id': cohort_id, 'limit': 1})
    check(insights['cohort']['enrollment_count'] == 2 and len(insights['learners']) == 1 and insights['next_after'], 'progress roster pagination')
    second = tool(trainer, 'get_cohort_insights', {'cohort_id': cohort_id, 'after': insights['next_after'], 'limit': 1})
    check(len(second['learners']) == 1 and not second['next_after'] and
          second['learners'][0]['enrollment_id'] != insights['learners'][0]['enrollment_id'], 'progress second page')
    check(all(c['average_mastery'] is None for c in insights['concepts']), 'small cohort average exposed')
    before_progress = tool(trainer, 'get_learner_progress', {'enrollment_id': enrollments[0]['id']})

    check(tool(trainer, 'get_learner_badges', {'enrollment_id': enrollments[0]['id']})['items'] == [], 'staff badge evidence')
    status, _, page = trainer_browser.get('/console/progress/badges?enrollment_id=' + enrollments[0]['id'])
    check(status == 200 and 'No badges earned' in page, 'staff badge browser read')
    check(before_progress['learner']['observed_concept_count'] >= 1 and before_progress['interaction_count'] == 1
          and before_progress['evaluated_attempt_count'] == 1 and before_progress['trusted_evaluation_count'] == 0
          and before_progress['synthesis_guidance'], 'individual evidence provenance')
    status, _, page = trainer_browser.get('/console/progress?enrollment_id=' + enrollments[0]['id'])
    check(status == 200 and 'alice@acme.test' in page and 'Concept progress' in page, 'trainer browser progress')
    status, _, page = manager.get('/console/progress?cohort_id=' + cohort_id)
    status, _, _ = manager.post('/console/progress/trainers', {
        'csrf_token': csrf(page), 'cohort_id': cohort_id, 'email': 'trainer@acme.test', 'assigned': 'false'})
    check(status == 200, 'trainer revocation failed')
    denied = trainer('tools/call', {'name': 'get_learner_progress', 'arguments': {'enrollment_id': enrollments[0]['id']}})
    check(denied.get('isError') and 'not_found' in json.dumps(denied), 'revoked trainer token retained progress access')
    denied = trainer('tools/call', {'name': 'get_learner_badges', 'arguments': {'enrollment_id': enrollments[0]['id']}})
    check(denied.get('isError') and 'not_found' in json.dumps(denied), 'revoked trainer read badges')
    status, _, page = manager.get('/console/progress?cohort_id=' + cohort_id)
    status, _, _ = manager.post('/console/progress/trainers', {
        'csrf_token': csrf(page), 'cohort_id': cohort_id, 'email': 'trainer@acme.test', 'assigned': 'true'})
    check(status == 200, 'trainer reassignment failed')
    print('PASS: staff progress OAuth, cohort insights, individual evidence, console dashboard and assignment revocation')
    print('PASS: learner and staff badge reads, no awards from practice, enrollment isolation and live revocation')

    # Leave/rejoin through both transports preserves the domain and evidence.
    for name, token, enrollment in [('alice', learner_token, enrollments[0]), ('bob', bob_token, enrollments[1])]:
        learner = mcp_client(token)
        if name == 'alice':
            tool(learner, 'leave_formation', {'cohort_id': cohort_id, 'idempotency_key': 'self-leave-alice'})
        else:
            _, _, page = bob_browser.get('/learn?mine=1')
            status, _, _ = bob_browser.post('/learn/enrollment', {
                'csrf_token': csrf(page), 'cohort_id': cohort_id, 'action': 'leave', 'idempotency_key': 'web-leave-bob'})
            check(status == 200, 'web leave failed')
        status, cohorts = console_api(manager, 'GET', '/admin/catalog/cohorts')
        check(status == 200 and next(c['reserved_seats'] for c in cohorts['items'] if c['id'] == cohort_id) == 1,
              'leave did not release exactly one seat')
        denied = learner('tools/call', {'name': 'unarchive_domain', 'arguments': {'domain_id': enrollment['domain_id']}})
        check(denied.get('isError'), 'unarchive bypassed cancelled enrollment')
        resumed = tool(learner, 'join_formation', {'cohort_id': cohort_id, 'idempotency_key': 'rejoin-' + name})['enrollment']
        check(resumed['id'] == enrollment['id'] and resumed['domain_id'] == enrollment['domain_id'], 'rejoin replaced learning history')
    status, after_report = console_api(manager, 'GET', f'/admin/catalog/cohorts/{cohort_id}/report')
    check(status == 200 and after_report == report, 'leave/rejoin changed shared progress report')
    status, _, page = bob_browser.get('/learn/formations/' + cohort_id)
    check(status == 200 and 'Read a variable' in page and 'Call a function' in page, 'published web program missing')
    print('PASS: learner MCP and web catalog, invitation joins, released seats, preserved progress and rejoin')

    # Approval requests reserve no seat and require an authorized staff decision.
    status, _ = console_api(manager, 'PUT', f'/admin/catalog/formations/{formation_id}/enrollment-policy', {'policy': 'approval'})
    check(status == 200, 'approval policy')
    status, approval_cohort = console_api(manager, 'POST', '/admin/catalog/cohorts',
        {'formation_version_id': version, 'name': 'Approval cohort', 'capacity': 1}, 'approval-cohort')
    check(status == 201, 'approval cohort creation')
    approval_id = approval_cohort['cohort']['id']
    bob = mcp_client(bob_token)
    pending = tool(bob, 'join_formation', {'cohort_id': approval_id, 'idempotency_key': 'approval-request'})
    check(pending['status'] == 'pending' and not pending.get('enrollment'), 'request enrolled before approval')
    status, admissions = console_api(manager, 'GET', f'/admin/catalog/cohorts/{approval_id}/admissions')
    check(status == 200 and len(admissions['items']) == 1, 'pending request missing')
    status, _, page = manager.get('/console/admissions?cohort_id=' + approval_id)
    check(status == 200 and 'bob@acme.test' in page, 'console admission page missing request')
    status, _, _ = manager.post('/console/admissions', {
        'csrf_token': csrf(page), 'cohort_id': approval_id, 'membership_id': claims(bob_token)['membership_id'],
        'decision': 'approved', 'expected_version': 1, 'idempotency_key': 'approve-bob'})
    check(status == 200, 'console approval failed')
    approved = tool(bob, 'join_formation', {'cohort_id': approval_id, 'idempotency_key': 'approval-confirm'})
    check(approved['status'] == 'active', 'approved join failed')
    tool(bob, 'leave_formation', {'cohort_id': approval_id, 'idempotency_key': 'approval-leave'})
    print('PASS: approval request and console decision followed by learner confirmation')

    clone = tool(author, 'draft_formation', {'source_version_id': version, 'idempotency_key': 'clone'})
    next_version = clone['version']['id']
    tool(author, 'publish_formation', {'version_id': next_version, 'idempotency_key': 'publish-next'})
    status, next_cohort = console_api(manager, 'POST', '/admin/catalog/cohorts',
        {'formation_version_id': next_version, 'name': 'Next version', 'capacity': 10}, 'next-cohort')
    check(status == 201, f'next cohort {status}')
    status, migration = console_api(manager, 'POST', f"/admin/catalog/enrollments/{enrollments[0]['id']}/migrate",
        {'cohort_id': next_cohort['cohort']['id']})
    check(status == 200 and migration['enrollment']['formation_version_id'] == next_version, f'migration {status}: {migration}')
    historical = tool(trainer, 'get_learner_progress', {'enrollment_id': enrollments[0]['id']})
    check(historical['cohort']['version_id'] == version and historical['learner']['status'] == 'cancelled'
          and historical['learner']['average_mastery'] == before_progress['learner']['average_mastery'], 'historical progress changed after migration')
    denied = trainer('tools/call', {'name': 'get_learner_progress', 'arguments': {'enrollment_id': migration['enrollment']['id']}})
    check(denied.get('isError'), 'trainer followed a migrated domain into an unassigned cohort')
    status, archived = console_api(manager, 'DELETE', f'/admin/catalog/formations/{formation_id}')
    check(status == 200 and archived['status'] == 'archived', f'archive {status}: {archived}')
    print('PASS: cloned version, explicit enrollment migration and non-destructive formation archive')

    # 3. Self-service signup of a second institution.
    founder = Browser()
    status, _, page = founder.get('/signup')
    check(status == 200, f'signup page {status}')
    status, _, page = founder.post('/signup', {
        'csrf_token': csrf(page), 'tenant_name': 'Beta School', 'tenant_slug': 'beta-school',
        'email': 'founder@beta.test', 'accept_terms': 'yes'})
    check(status == 202, f'signup request {status}')
    link = mail_link('/signup/complete', seen_mail)
    status, _, page = founder.get(local_path(link))
    check(status == 200, f'signup confirmation page {status}')
    status, path, page = founder.post('/signup/complete', {
        'csrf_token': csrf(page), 'token': field(r'name="token" value="([^"]+)"', page, 'signup token'),
        'password': 'founder-password-2026', 'password_confirm': 'founder-password-2026'})
    check(path == '/console/mfa/setup', f'founder landed on {path} ({status})')
    enroll_totp(founder, page)
    status, _, page = founder.get('/console')
    check(status == 200 and 'Beta School' in page, f'founder console {status}')
    print('PASS: self-service institution signup with email confirmation and TOTP')


if __name__ == '__main__':
    main()
