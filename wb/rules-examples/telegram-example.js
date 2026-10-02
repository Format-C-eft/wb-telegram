// Пример правил для wb-telegram. Скопируйте в /etc/wb-rules/ и поправьте под свой дом.
// Команды gate и boiler должны быть заведены в настройках бота (веб-интерфейс → Конфигурационные файлы → Telegram-бот).

var tg = require("telegram");

// /gate — открыть ворота (замените канал реле на свой).
tg.onCommand("gate", function (cmd) {
  dev["wb-gpio/EXT1_R3A1"] = true;
  cmd.reply("Ворота открываются (" + cmd.user + ")");
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
