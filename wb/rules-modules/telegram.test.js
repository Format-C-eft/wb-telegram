const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const modulePath = path.join(__dirname, 'telegram.js');

function createSandbox() {
  let now = 2000000000000;
  const rules = {};
  const published = [];
  const warnings = [];
  const dev = new Proxy({}, {
    set(target, key, value) {
      target[key] = value;
      published.push({ key, value: JSON.parse(value) });
      return true;
    }
  });
  const sandbox = {
    Date: class extends Date {
      static now() { return now; }
    },
    Math,
    JSON,
    String,
    Object,
    Array,
    isNaN,
    dev,
    defineRule(name, rule) { rules[name] = rule; },
    log: { warning(...args) { warnings.push(args); } }
  };
  const context = vm.createContext(sandbox);
  const source = fs.readFileSync(modulePath, 'utf8');
  const wrapper = `(function(exports, require, module) { ${source}\n})`;
  const factory = vm.runInContext(wrapper, context, { filename: modulePath });
  const module = { exports: {} };
  factory(module.exports, () => { throw new Error('unexpected require'); }, module);

  return {
    api: module.exports,
    published,
    rules,
    warnings,
    setNow(value) { now = value; }
  };
}

function lastPayload(sandbox) {
  return sandbox.published[sandbox.published.length - 1].value;
}

test('legacy text methods keep their payload shape and routing', () => {
  const sandbox = createSandbox();

  sandbox.api.send('hello');
  assert.deepEqual(lastPayload(sandbox), { text: 'hello', id: lastPayload(sandbox).id });
  sandbox.api.sendTo(['Alice', 'Bob'], 'private');
  assert.deepEqual(lastPayload(sandbox), { text: 'private', to: ['Alice', 'Bob'], id: lastPayload(sandbox).id });
});

test('plain text methods accept button options without changing routing', () => {
  const sandbox = createSandbox();
  const options = { buttons: [[{ text: 'OK', command: 'confirm' }]], parse_mode: 'HTML' };

  sandbox.api.send('plain', options);
  assert.equal(lastPayload(sandbox).parse_mode, undefined);
  assert.deepEqual(lastPayload(sandbox).buttons, [[{ text: 'OK', command: 'confirm' }]]);
  sandbox.api.sendTo('Alice', 'private', options);
  assert.deepEqual(lastPayload(sandbox).to, ['Alice']);
  assert.equal(lastPayload(sandbox).parse_mode, undefined);
  assert.deepEqual(lastPayload(sandbox).buttons, [[{ text: 'OK', command: 'confirm' }]]);
});

test('format methods select HTML and MarkdownV2 without public parse mode', () => {
  const sandbox = createSandbox();

  sandbox.api.sendHTML('<b>yes</b>');
  assert.equal(lastPayload(sandbox).parse_mode, 'HTML');
  sandbox.api.sendMarkdown('*yes*');
  assert.equal(lastPayload(sandbox).parse_mode, 'MarkdownV2');
  sandbox.api.sendToHTML('Alice', '<b>yes</b>');
  assert.deepEqual(lastPayload(sandbox).to, ['Alice']);
  sandbox.api.sendToMarkdown(['Alice'], '*yes*');
  assert.deepEqual(lastPayload(sandbox).to, ['Alice']);
  assert.equal(lastPayload(sandbox).parse_mode, 'MarkdownV2');
});

test('escape helpers escape markup characters', () => {
  const sandbox = createSandbox();

  assert.equal(sandbox.api.escapeHTML('&<>"\''), '&amp;&lt;&gt;&quot;&#39;');
  assert.equal(sandbox.api.escapeMarkdownV2('_*[]()~`>#+-=|{}.!\\'), '\\_\\*\\[\\]\\(\\)\\~\\`\\>\\#\\+\\-\\=\\|\\{\\}\\.\\!\\\\');
});

test('buttons are copied and caller routing fields cannot override wrappers', () => {
  const sandbox = createSandbox();
  const button = { text: 'Open', command: 'gate', args: 'open', to: ['Mallory'], id: 'bad' };
  const buttons = [[button, { text: 'Cancel', cancel: true }]];
  const options = { buttons, to: ['Mallory'], reply_to: 'bad', id: 'bad', parse_mode: 'Markdown' };

  sandbox.api.sendHTML('<b>Choose</b>', options);
  assert.equal(lastPayload(sandbox).parse_mode, 'HTML');
  assert.equal(lastPayload(sandbox).to, undefined);
  assert.equal(lastPayload(sandbox).reply_to, undefined);
  assert.notEqual(lastPayload(sandbox).id, 'bad');
  assert.deepEqual(lastPayload(sandbox).buttons, [[
    { text: 'Open', command: 'gate', args: 'open' },
    { text: 'Cancel', cancel: true }
  ]]);
  assert.equal(button.to[0], 'Mallory');
});

test('malformed button rows fail with a descriptive error', () => {
  const sandbox = createSandbox();

  assert.throws(() => sandbox.api.send('plain', { buttons: [null] }), /каждая строка buttons должна быть массивом/);
  assert.throws(() => sandbox.api.send('plain', { buttons: ['bad row'] }), /каждая строка buttons должна быть массивом/);
  assert.throws(() => sandbox.api.send('plain', { buttons: [[null]] }), /кнопка должна быть объектом/);
});

test('reply and chart APIs apply module routing and assign message ids', () => {
  const sandbox = createSandbox();
  const now = 2000000000000;
  sandbox.setNow(now);
  sandbox.api.onCommand('graph', function (cmd) {
    cmd.reply('plain', { buttons: [[{ text: 'More', command: 'graph' }]], parse_mode: 'HTML', reply_to: 'wrong' });
    cmd.replyHTML('<b>formatted</b>');
    cmd.replyMarkdown('*formatted*');
    cmd.replyChart({ title: 'History', period_s: 3600, channels: [{ device: 'boiler', control: 'temperature', label: 'Feed', unit: 'C' }] });
  });
  const rule = sandbox.rules.telegram_command_graph;
  rule.then(JSON.stringify({ id: 'call-1', ts: now / 1000, args: '' }));
  assert.equal(sandbox.published[0].value.reply_to, 'call-1');
  assert.equal(sandbox.published[0].value.parse_mode, undefined);
  assert.deepEqual(sandbox.published[0].value.buttons, [[{ text: 'More', command: 'graph' }]]);
  assert.equal(sandbox.published[1].value.parse_mode, 'HTML');
  assert.equal(sandbox.published[2].value.parse_mode, 'MarkdownV2');
  assert.deepEqual(sandbox.published[3].value.chart, { title: 'History', period_s: 3600, channels: [{ device: 'boiler', control: 'temperature', label: 'Feed', unit: 'C' }] });
  assert.equal(sandbox.published[3].value.reply_to, 'call-1');
  assert.equal(new Set(sandbox.published.map((item) => item.value.id)).size, 4);
  sandbox.api.sendChart({ title: 'all' });
  assert.deepEqual(lastPayload(sandbox).chart, { title: 'all' });
  sandbox.api.sendChartTo('Alice', { title: 'one' });
  assert.deepEqual(lastPayload(sandbox).to, ['Alice']);
});

test('commands reject missing, stale, retained and duplicate ids', () => {
  const sandbox = createSandbox();
  const now = 2000000000000;
  let calls = 0;
  sandbox.api.onCommand('ping', function () { calls += 1; });
  const then = sandbox.rules.telegram_command_ping.then;

  then(JSON.stringify({ id: 'old', ts: now / 1000 - 61 }));
  then(JSON.stringify({ id: 'retained', ts: now / 1000 - 2 }));
  then(JSON.stringify({ id: 'fresh', ts: now / 1000 }));
  then(JSON.stringify({ id: 'fresh', ts: now / 1000 }));
  then('{');
  assert.equal(calls, 1);
  assert.equal(sandbox.warnings.length, 1);
});

test('example includes callback-driven command pattern', () => {
  const example = fs.readFileSync(path.join(__dirname, '../rules-examples/telegram-example.js'), 'utf8');

  assert.match(example, /buttons:\s*\[\[/);
  assert.match(example, /cancel:\s*true/);
  assert.match(example, /tg\.onCommand\("gate"/);
});
