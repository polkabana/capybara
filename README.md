# Capybara DMR Server

English version will be later, use online translators now.

DMR сервер для локальных нужд. Используется протокол Homewbrew Brandmeister. Поддержка приватных вызовов, разговорных групп, встроенный автоответчик-попугай.
Написан на языке Go.


## Установка

```
mkdir /opt/capybara
cd /opt/capybara
git clone https://github.com/polkabana/capybara
go get github.com/polkabana/go-dmr github.com/op/go-logging gopkg.in/gcfg.v1
go build capybara.go
```


## Запуск

`# ./capybara.go`

Или через сервисы

`# sudo nano /lib/systemd/system/capybara.service`

```
[Unit]
Description=Capybara Service

After=network-online.target syslog.target
Wants=network-online.target

[Service]
StandardOutput=null
WorkingDirectory=/opt/capybara
RestartSec=3
ExecStart=/opt/capybara/capybara > /dev/null 2> /dev/null
Restart=on-abort

[Install]
WantedBy=multi-user.target
```

`# sudo systemctl restart capybara`


## Настройка

При запуске ищется файл capybara.cfg в текущей директории, содержимое которого определяет настройки сервера. Пример файла:

```
[General]
# ID сервера в сети
ID = 1
# IP адрес интерфейса, который слушает сервер
IP = 0.0.0.0
# Порт по умолчанию 62031
Port = 62031
# Пароль к серверу
Password = passw0rd
# Полное имя лог-файла
LogName = /var/log/capybara.log
# Уровень детализацуии лога (CRITICAL, ERROR, WARNING, NOTICE, INFO, DEBUG), по-умолчанию ERROR
LogLevel = DEBUG
# Разрешить работу HTTP сервера с информацией о DMR сервере
EnableHTTP = 1, по-умолчанию 0 - выключен
# Порт HTTP сервера
HTTPPort = 8080
# База DMR IDs, например, http://registry.dstar.su/dmr/DMRIds2.php
DMRIDs = DMRIds2.php

[Groups]
# Поддержка приватных вызовов, по-умолчанию 0 - выключена
EnablePC = 0
# Список доступных разговорных групп
AvailableTG = 433, 434, 446
# Разговорная группа по-умолчанию
DefaultTG = 446
# Разговорная группа попугая
ParrotTG = 9999
```


## Группы

После подключения клиента к серверу, он будет принудительно подписан на DefaultTG. Для подписки на другую разговорную группу необходимо совершить вызов в эту группу.


## Попугай

Эхо-репитер, т.н. попугай, реализован в виде разговорной группы. Ответ попугая слышит только тот узел, который обращался. Другие узлы не слышат.


## Web Монитор

При обращеннии к IP адресу и порту HTTPPort, доступен мониторинг состояния сервера: активные подключения и список последних вызовов. Позывные и имена пользователей берутся из файла указанного в настройке DMRIDs.


## Настройка pi-star

[Здесь](http://hblink.mqtt.by) пример как настроить pi-star для совместной работы Brandmeister и пользовательского DMR сервера
