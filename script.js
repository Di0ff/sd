(function () {
  'use strict';

  // Telegram Web App
  var tg = window.Telegram && window.Telegram.WebApp;
  var tgChatId = null;
  var tgUser = null;

  // Инициализация Telegram
  function initTelegram() {
    if (!tg) return;

    tg.ready();
    tg.expand();
    
    // Добавляем класс для Telegram Web App
    document.body.classList.add('telegram-webapp');

    // Сохраняем данные пользователя
    if (tg.initDataUnsafe && tg.initDataUnsafe.user) {
      tgUser = tg.initDataUnsafe.user;
      tgChatId = tgUser.id;

      // Отправляем chat_id на бэкенд
      fetch('/api/tg/init', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          chat_id: tgChatId,
          first_name: tgUser.first_name,
          username: tgUser.username,
          phone: ''
        })
      }).catch(function(err) {
        console.log('Telegram init error:', err);
      });
    }
  }

  initTelegram();

  // Плавная прокрутка по якорям
  document.querySelectorAll('a[href^="#"]').forEach(function (anchor) {
    anchor.addEventListener('click', function (e) {
      var targetId = this.getAttribute('href');
      if (targetId === '#') return;
      e.preventDefault();
      var target = document.querySelector(targetId);
      if (target) target.scrollIntoView({ behavior: 'smooth', block: 'start' });
    });
  });

  // Маска телефона через IMask
  var phoneInput = document.getElementById('guest-phone');
  if (phoneInput) {
    IMask(
      phoneInput,
      {
        mask: '+{7} (000) 000-00-00',
        lazy: true
      }
    );
  }

  // Маска почты: только допустимые символы
  var emailInput = document.getElementById('guest-email');
  if (emailInput) {
    emailInput.addEventListener('input', function () {
      this.value = this.value.replace(/[^\w\u0400-\u04FF@.\-+]/g, '');
    });
  }

  // Форма: отправка на бэк /api/rsvp, при ошибке — сохранение в localStorage
  var form = document.getElementById('rsvp-form');
  var message = document.getElementById('rsvp-message');
  if (form && message) {
    form.addEventListener('submit', function (e) {
      e.preventDefault();
      var nameInput = document.getElementById('guest-name');
      var phoneInput = document.getElementById('guest-phone');
      var emailInput = document.getElementById('guest-email');
      var name = nameInput && nameInput.value ? nameInput.value.trim() : '';
      var phoneRaw = phoneInput && phoneInput.value ? phoneInput.value.replace(/\D/g, '') : '';
      var phone = phoneInput && phoneInput.value ? phoneInput.value.trim() : '';
      var email = emailInput && emailInput.value ? emailInput.value.trim() : '';
      if (!name || !phoneRaw) return;
      
      var payload = { 
        name: name, 
        phone: phone, 
        email: email || '',
        telegram_chat_id: tgChatId || null
      };
      
      fetch('/api/rsvp', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload)
      }).then(function (res) {
        if (res.ok) {
          message.textContent = 'Спасибо! Рады, что вы будете с нами. Ждём на празднике!';
          message.classList.remove('rsvp-form__message--error');
          message.classList.add('is-visible');
          form.reset();
          return;
        }
        return res.json().then(function (data) {
          throw new Error(data.error || res.statusText);
        }).catch(function () {
          throw new Error(res.statusText);
        });
      }).catch(function (err) {
        try {
          var list = JSON.parse(localStorage.getItem('wedding_rsvp') || '[]');
          list.push({ name: name, phone: phone, phoneRaw: phoneRaw, email: email || undefined, telegram_chat_id: tgChatId || null, at: new Date().toISOString() });
          localStorage.setItem('wedding_rsvp', JSON.stringify(list));
        } catch (e) {}
        message.textContent = 'Не удалось отправить. Попробуйте позже или свяжитесь с нами по телефону.';
        message.classList.add('rsvp-form__message--error');
        message.classList.add('is-visible');
      });
    });
  }
})();
