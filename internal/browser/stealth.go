package browser

import (
	"github.com/go-rod/rod"
)

// applyStealth injects anti-detection patches into the page.
func applyStealth(page *rod.Page) {
	log.Debugf("Applying stealth patches")

	// Remove navigator.webdriver flag
	_, _ = page.Eval(`() => {
		Object.defineProperty(navigator, 'webdriver', {
			get: () => undefined,
		});
	}`)

	// Fake plugins array
	_, _ = page.Eval(`() => {
		Object.defineProperty(navigator, 'plugins', {
			get: () => [
				{ name: 'Chrome PDF Plugin', filename: 'internal-pdf-viewer' },
				{ name: 'Chrome PDF Viewer', filename: 'mhjfbmdgcfjbbpaeojofohoefgiehjai' },
				{ name: 'Native Client', filename: 'internal-nacl-plugin' },
			],
		});
	}`)

	// Fake languages
	_, _ = page.Eval(`() => {
		Object.defineProperty(navigator, 'languages', {
			get: () => ['en-US', 'en'],
		});
	}`)

	// Fix chrome.runtime to avoid detection
	_, _ = page.Eval(`() => {
		if (!window.chrome) {
			window.chrome = {};
		}
		if (!window.chrome.runtime) {
			window.chrome.runtime = {};
		}
	}`)

	// Mask automation-related properties
	_, _ = page.Eval(`() => {
		// Remove automation indicators from window.navigator
		const props = ['hardwareConcurrency', 'deviceMemory', 'maxTouchPoints'];
		for (const prop of props) {
			if (navigator[prop] !== undefined) {
				Object.defineProperty(navigator, prop, {
					get: () => navigator[prop],
				});
			}
		}
	}`)

	// Override permissions query
	_, _ = page.Eval(`() => {
		const originalQuery = window.navigator.permissions.query;
		window.navigator.permissions.query = (parameters) =>
			parameters.name === 'notifications'
				? Promise.resolve({ state: Notification.permission })
				: originalQuery(parameters);
	}`)

	// WebGL vendor/renderer spoofing
	_, _ = page.Eval(`() => {
		const getParameter = WebGLRenderingContext.prototype.getParameter;
		WebGLRenderingContext.prototype.getParameter = function(parameter) {
			if (parameter === 37445) return 'Intel Inc.';
			if (parameter === 37446) return 'Intel Iris OpenGL Engine';
			return getParameter.call(this, parameter);
		};
	}`)

	// Canvas fingerprint noise
	_, _ = page.Eval(`() => {
		const originalToDataURL = HTMLCanvasElement.prototype.toDataURL;
		HTMLCanvasElement.prototype.toDataURL = function(type) {
			if (type === 'image/webp') {
				return originalToDataURL.apply(this, arguments);
			}
			const context = this.getContext('2d');
			if (context) {
				const pixel = context.getImageData(0, 0, 1, 1);
				pixel.data[0] = pixel.data[0] ^ 1;
				context.putImageData(pixel, 0, 0);
			}
			return originalToDataURL.apply(this, arguments);
		};
	}`)

	log.Debugf("Stealth patches applied")
}
