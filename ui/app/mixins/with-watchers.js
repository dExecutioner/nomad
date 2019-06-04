import Mixin from '@ember/object/mixin';
import { computed } from '@ember/object';
import { assert } from '@ember/debug';
import { inject as service } from '@ember/service';
import WithVisibilityDetection from './with-route-visibility-detection';

export default Mixin.create(WithVisibilityDetection, {
  flashMessages: service(),

  watchers: computed(() => []),

  init() {
    this._super(...arguments);
    this.displayedFlashMessages = [];
  },

  cancelAllWatchers() {
    this.watchers.forEach(watcher => {
      assert('Watchers must be Ember Concurrency Tasks.', !!watcher.cancelAll);
      watcher.cancelAll();
    });
  },

  removeFlashMessages() {
    this.displayedFlashMessages.forEach(message => this.flashMessages.queue.removeObject(message));
    this.displayedFlashMessages = [];
  },

  startWatchers() {
    assert('startWatchers needs to be overridden in the Route', false);
  },

  setupController() {
    this.startWatchers(...arguments);
    return this._super(...arguments);
  },

  visibilityHandler() {
    if (document.hidden) {
      this.cancelAllWatchers();
    } else {
      this.startWatchers(this.controller, this.controller.get('model'));
    }
  },

  actions: {
    willTransition(transition) {
      // Don't cancel watchers if transitioning into a sub-route
      if (!transition.intent.name || !transition.intent.name.startsWith(this.routeName)) {
        this.cancelAllWatchers();
        this.removeFlashMessages();
      }

      // Bubble the action up to the application route
      return true;
    },
  },
});
