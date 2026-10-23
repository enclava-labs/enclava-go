(function () {
  function attach(root) {
    var form = root.querySelector('#connector-create-form');
    if (!form || form.dataset.bound) return;
    form.dataset.bound = 'true';
    var type = form.elements.type, credential = form.elements.credential_ref;
    function filter() {
      var prefix = 'CONNECTOR_' + String(type.value).toUpperCase() + '_';
      var selected = false;
      Array.prototype.forEach.call(credential.options, function (option) {
        option.hidden = option.value !== '' && option.value.indexOf(prefix) !== 0;
        option.disabled = option.hidden;
        if (option.value === credential.value && !option.hidden) selected = true;
      });
      if (!selected) credential.value = '';
    }
    type.addEventListener('change', filter);
    filter();
  }
  document.addEventListener('DOMContentLoaded', function () { attach(document); });
  document.addEventListener('htmx:afterSwap', function (event) { attach(event.target); });
})();
