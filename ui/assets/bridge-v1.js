// UI Bridge v1 客户端：与宿主页面通过 postMessage 通信。
// 宿主为每次打开配置页生成 bridge_token，只放在 URL fragment 里。
(function () {
  "use strict";

  var UI_SOURCE = "sub2api-plugin-ui";
  var HOST_SOURCE = "sub2api-plugin-host";
  var TIMEOUT_MS = 30000;

  var token = new URLSearchParams(window.location.hash.slice(1)).get("bridge_token") || "";
  var pending = new Map();
  var sequence = 0;

  function nextRequestID() {
    sequence += 1;
    return "bps-" + Date.now().toString(36) + "-" + sequence.toString(36);
  }

  function post(type, payload) {
    var message = Object.assign({}, payload || {}, {
      source: UI_SOURCE,
      bridge_token: token,
      type: type
    });
    // sandbox iframe 的父页面来源不可预知，只能用 "*"；宿主会校验 source 窗口与 token。
    window.parent.postMessage(message, "*");
  }

  function request(type, payload) {
    if (!token) {
      return Promise.reject(new Error("缺少 bridge_token，请从插件管理页打开"));
    }
    var requestID = nextRequestID();
    return new Promise(function (resolve, reject) {
      var timer = window.setTimeout(function () {
        pending.delete(requestID);
        reject(new Error("宿主响应超时"));
      }, TIMEOUT_MS);
      pending.set(requestID, { resolve: resolve, reject: reject, timer: timer, type: type });
      post(type, Object.assign({}, payload || {}, { request_id: requestID }));
    });
  }

  window.addEventListener("message", function (event) {
    if (event.source !== window.parent) return;
    var data = event.data;
    if (!data || data.source !== HOST_SOURCE || data.bridge_token !== token) return;
    var entry = pending.get(data.request_id);
    if (!entry || data.type !== entry.type + ".result") return;
    pending.delete(data.request_id);
    window.clearTimeout(entry.timer);
    if (data.ok) {
      entry.resolve(data);
    } else {
      var detail = data.error || (data.result && data.result.message) || "操作失败";
      var error = new Error(detail);
      error.result = data.result;
      entry.reject(error);
    }
  });

  window.addEventListener("unload", function () {
    pending.forEach(function (entry) {
      window.clearTimeout(entry.timer);
      entry.reject(new Error("页面已卸载"));
    });
    pending.clear();
  });

  window.S2PBridge = {
    ready: function () { post("sub2api.plugin.ready"); },
    loadConfig: function () { return request("config.load").then(function (r) { return r.config || {}; }); },
    saveConfig: function (config) { return request("config.save", { config: config }).then(function (r) { return r.config || config; }); },
    testConfig: function () { return request("config.test").then(function (r) { return r.result || {}; }); },
    status: function () { return request("plugin.status").then(function (r) { return r.result || {}; }); },
    resize: function (height) { post("ui.resize", { height: height }); },
    notify: function (level, message) { post("ui.notify", { level: level, message: String(message).slice(0, 500) }); }
  };
})();
