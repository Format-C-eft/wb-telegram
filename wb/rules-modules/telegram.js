// Модуль wb-rules для Telegram-бота wb-telegram.
//
//   var tg = require("telegram");
//   tg.onCommand("gate", function (cmd) { cmd.reply("Открываю"); });
//   tg.send("Котёл: ошибка");

var DEVICE = "telegram_bot";
var MAX_AGE_S = 60;
var MAX_SEEN = 100;
var sequence = 0;
var loadedAt = Date.now() / 1000;

// nextId возвращает уникальный id сообщения.
function nextId() {
  sequence += 1;

  return "wbr-" + Date.now() + "-" + sequence + "-" + Math.floor(Math.random() * 1000000);
}

// copyButtons копирует описание кнопок без ссылок на объект вызывающего кода.
function copyButtons(buttons) {
  var rows = [];
  var i;
  var j;
  var button;
  var copy;

  if (!Array.isArray(buttons)) {
    throw new Error("telegram: buttons должны быть массивом строк");
  }

  for (i = 0; i < buttons.length; i += 1) {
    if (!Array.isArray(buttons[i])) {
      throw new Error("telegram: каждая строка buttons должна быть массивом");
    }

    rows[i] = [];
    for (j = 0; j < buttons[i].length; j += 1) {
      button = buttons[i][j];
      if (!button || typeof button !== "object") {
        throw new Error("telegram: кнопка должна быть объектом");
      }

      copy = { text: button.text };
      if (button.command !== undefined) {
        copy.command = button.command;
      }
      if (button.args !== undefined) {
        copy.args = button.args;
      }
      if (button.cancel !== undefined) {
        copy.cancel = button.cancel;
      }
      rows[i][j] = copy;
    }
  }

  return rows;
}

// publish публикует payload с назначенными модулем служебными полями.
function publish(message, options, routing) {
  var payload = {};
  var key;

  for (key in message) {
    if (Object.prototype.hasOwnProperty.call(message, key)) {
      payload[key] = message[key];
    }
  }

  if (options && options.buttons) {
    payload.buttons = copyButtons(options.buttons);
  }

  if (routing.to !== undefined) {
    payload.to = routing.to;
  }
  if (routing.reply_to !== undefined) {
    payload.reply_to = routing.reply_to;
  }

  payload.id = nextId();
  dev[DEVICE + "/send"] = JSON.stringify(payload);
}

// rememberId запоминает обработанный id.
function rememberId(seen, id) {
  if (seen.index[id]) {
    return true;
  }

  seen.index[id] = true;
  seen.order.push(id);

  if (seen.order.length > MAX_SEEN) {
    delete seen.index[seen.order.shift()];
  }

  return false;
}

// escapeHTML экранирует текст для HTML-разметки.
exports.escapeHTML = function (value) {
  return String(value).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/\"/g, "&quot;").replace(/'/g, "&#39;");
};

// escapeMarkdownV2 экранирует обычный текст вне code/pre/URL.
exports.escapeMarkdownV2 = function (value) {
  return String(value).replace(/[\\_\*\[\]()~`>#+\-=|{}.!]/g, "\\$&");
};

// send отправляет обычный текст всем пользователям с системными уведомлениями.
exports.send = function (text, options) {
  publish({ text: String(text) }, options, {});
};

// sendTo отправляет обычный текст выбранным пользователям.
exports.sendTo = function (users, text, options) {
  publish({ text: String(text) }, options, { to: [].concat(users) });
};

// sendHTML отправляет текст с HTML-разметкой.
exports.sendHTML = function (text, options) {
  publish({ text: String(text), parse_mode: "HTML" }, options, {});
};

// sendMarkdown отправляет текст с MarkdownV2-разметкой.
exports.sendMarkdown = function (text, options) {
  publish({ text: String(text), parse_mode: "MarkdownV2" }, options, {});
};

// sendToHTML отправляет HTML-текст выбранным пользователям.
exports.sendToHTML = function (users, text, options) {
  publish({ text: String(text), parse_mode: "HTML" }, options, { to: [].concat(users) });
};

// sendToMarkdown отправляет MarkdownV2-текст выбранным пользователям.
exports.sendToMarkdown = function (users, text, options) {
  publish({ text: String(text), parse_mode: "MarkdownV2" }, options, { to: [].concat(users) });
};

// sendChart отправляет спецификацию графика всем пользователям с системными уведомлениями.
exports.sendChart = function (spec) {
  publish({ chart: spec }, null, {});
};

// sendChartTo отправляет спецификацию графика выбранным пользователям.
exports.sendChartTo = function (users, spec) {
  publish({ chart: spec }, null, { to: [].concat(users) });
};

// onCommand регистрирует обработчик команды /name.
exports.onCommand = function (name, handler) {
  var seen = { index: {}, order: [] };

  defineRule("telegram_command_" + name, {
    whenChanged: DEVICE + "/cmd_" + name,
    then: function (newValue) {
      var call;
      var age;
      var now;

      if (!newValue) {
        return;
      }

      try {
        call = JSON.parse(newValue);
      } catch (e) {
        log.warning("telegram: некорректный вызов /{}: {}", name, newValue);
        return;
      }

      if (!call || !call.id || typeof call.ts !== "number" || !isFinite(call.ts)) {
        return;
      }

      now = Date.now() / 1000;
      loadedAt = Math.min(loadedAt, now);
      age = now - call.ts;
      if (call.ts < loadedAt - 1 || age > MAX_AGE_S || age < -1) {
        return;
      }

      if (rememberId(seen, call.id)) {
        return;
      }

      handler({
        id: call.id,
        user: call.user,
        args: call.args || "",
        canReplyEphemeral: call.can_reply_ephemeral === true,
        reply: function (text, options) {
          publish({ text: String(text) }, options, { reply_to: call.id });
        },
        replyHTML: function (text, options) {
          publish({ text: String(text), parse_mode: "HTML" }, options, { reply_to: call.id });
        },
        replyMarkdown: function (text, options) {
          publish({ text: String(text), parse_mode: "MarkdownV2" }, options, { reply_to: call.id });
        },
        replyChart: function (spec) {
          publish({ chart: spec }, null, { reply_to: call.id });
        },
        replyEphemeral: function (text) {
          if (call.can_reply_ephemeral !== true) {
            throw new Error("telegram: команда не поддерживает временный ответ");
          }

          publish({ text: String(text), ephemeral: true }, null, { reply_to: call.id });
        },
        replyEphemeralChart: function (spec) {
          if (call.can_reply_ephemeral !== true) {
            throw new Error("telegram: команда не поддерживает временный ответ");
          }

          publish({ chart: spec, ephemeral: true }, null, { reply_to: call.id });
        },
        replyBoilerWidget: function (spec) {
          if (name !== "boiler_status") {
            throw new Error("telegram: карточка котла доступна только для boiler_status");
          }

          publish({ boiler_widget: {
            temperature: String(spec.temperature),
            status: String(spec.status)
          } }, null, { reply_to: call.id });
        }
      });
    }
  });
};
