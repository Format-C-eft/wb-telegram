// Модуль wb-rules для Telegram-бота wb-telegram.
//
//   var tg = require("telegram");
//   tg.onCommand("gate", function (cmd) { cmd.reply("Открываю"); });
//   tg.send("Котёл: ошибка");               // всем с системными уведомлениями
//   tg.sendTo(["Супруга"], "Курьер у ворот"); // конкретным людям

var DEVICE = "telegram_bot";
var MAX_AGE_S = 60;
var MAX_SEEN = 100;
var sequence = 0;

// Время загрузки модуля: после перезапуска wb-rules память о обработанных id пуста,
// поэтому вызовы, пришедшие до загрузки, считаются устаревшими и не выполняются повторно.
var loadedAt = Date.now() / 1000;

// nextId возвращает уникальный id сообщения, чтобы одинаковые тексты подряд не схлопывались.
function nextId() {
  sequence += 1;

  return "wbr-" + Date.now() + "-" + sequence + "-" + Math.floor(Math.random() * 1000000);
}

// publish пишет сообщение в контрол send бота.
function publish(message) {
  message.id = nextId();
  dev[DEVICE + "/send"] = JSON.stringify(message);
}

// rememberId запоминает обработанный id и сообщает, встречался ли он раньше.
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

// send отправляет текст всем пользователям с включёнными системными уведомлениями.
exports.send = function (text) {
  publish({ text: String(text) });
};

// sendTo отправляет текст пользователям по именам из настроек бота.
exports.sendTo = function (users, text) {
  publish({ text: String(text), to: [].concat(users) });
};

// onCommand регистрирует обработчик команды /name.
exports.onCommand = function (name, handler) {
  var seen = { index: {}, order: [] };

  defineRule("telegram_command_" + name, {
    whenChanged: DEVICE + "/cmd_" + name,
    then: function (newValue) {
      var call;
      var age;

      if (!newValue) {
        return;
      }

      try {
        call = JSON.parse(newValue);
      } catch (e) {
        log.warning("telegram: некорректный вызов /{}: {}", name, newValue);

        return;
      }

      if (!call || !call.id) {
        return;
      }

      // Отбрасываем вызовы без числового ts, старше MAX_AGE_S и появившиеся до загрузки модуля (допуск 1 с).
      age = Date.now() / 1000 - call.ts;

      if (!(call.ts >= loadedAt - 1) || !(age <= MAX_AGE_S)) {
        return;
      }

      if (rememberId(seen, call.id)) {
        return;
      }

      handler({
        id: call.id,
        user: call.user,
        args: call.args || "",
        reply: function (text) {
          publish({ text: String(text), reply_to: call.id });
        }
      });
    }
  });
};
