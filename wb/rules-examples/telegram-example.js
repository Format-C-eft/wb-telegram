// Пример правил для wb-telegram. Скопируйте в /etc/wb-rules/ и поправьте под свой дом.
// Добавьте нужные команды в настройках бота (веб-интерфейс → Конфигурационные файлы → Telegram-бот).

var tg = require("telegram");

// /gate — открыть ворота (замените канал реле на свой).
tg.onCommand("gate", function (cmd) {
  if (cmd.args === "open") {
    dev["wb-gpio/EXT1_R3A1"] = true;
    cmd.reply("Команда открытия передана");

    return;
  }

  cmd.reply("Открыть ворота?", { buttons: [[
    { text: "Открыть", command: "gate", args: "open" },
    { text: "Отмена", cancel: true }
  ]] });
});

// /boiler_status показывает карточку; замените каналы на свои.
tg.onCommand("boiler_status", function (cmd) {
  var temperature = dev["boiler/flow_temperature"];
  var errorCode = dev["boiler/error_code"];
  var status;

  if (temperature === undefined || temperature === null || temperature === "") {
    temperature = "Недоступна";
  } else {
    temperature = String(temperature) + " °C";
  }

  if (errorCode === undefined || errorCode === null || errorCode === "") {
    status = "Нет данных о состоянии";
  } else if (String(errorCode) === "0") {
    status = "Ошибок нет";
  } else {
    status = "Ошибка " + String(errorCode);
  }

  cmd.replyBoilerWidget({ temperature: temperature, status: status });
});

// Пример запроса графика; канал иллюстративный, его нужно заменить на свой.
tg.onCommand("boiler_graph", function (cmd) {
  if (cmd.args === "menu") {
    cmd.reply("Показать историю температуры?", { buttons: [[
      { text: "Показать график", command: "boiler_graph" }
    ]] });

    return;
  }

  var chart = {
    title: "Температура котла",
    period_s: 86400,
    channels: [
      { device: "boiler", control: "flow_temperature", label: "Подача", unit: "°C" }
    ]
  };

  if (cmd.canReplyEphemeral) {
    cmd.replyEphemeralChart(chart);

    return;
  }

  cmd.replyChart(chart);
});

// /boiler — прислать состояние котла; /boiler 55 — задать температуру.
tg.onCommand("boiler", function (cmd) {
  if (cmd.args) {
    var target = parseFloat(cmd.args);

    if (isNaN(target) || target < 30 || target > 80) {
      cmd.reply("Нужна температура от 30 до 80, например: /boiler 55");

      return;
    }

    dev["boiler/target_temperature"] = target;
    cmd.reply("Задана температура " + target + " °C");

    return;
  }

  cmd.reply("Температура подачи: " + dev["boiler/flow_temperature"] + " °C");
});

// Системное уведомление: всем, у кого включены «Системные уведомления».
defineRule("telegram_example_boiler_error", {
  whenChanged: "boiler/error_code",
  then: function (newValue) {
    if (newValue) {
      tg.send("Котёл: ошибка " + newValue);
    }
  }
});
