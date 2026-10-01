/** Small shared helpers. */

/**
 * @param {Uint8Array} bytes
 * @returns {string} lowercase hex
 */
export function bytesToHex(bytes) {
	let out = '';
	for (let i = 0; i < bytes.length; i++) out += bytes[i].toString(16).padStart(2, '0');
	return out;
}

/**
 * Bound a promise that may neither resolve nor reject.
 *
 * @param {Promise<any>} promise
 * @param {number} ms
 * @param {string} message rejection message on timeout
 */
export function withTimeout(promise, ms, message) {
	return Promise.race([
		promise,
		new Promise((_, reject) => setTimeout(() => reject(new Error(message)), ms)),
	]);
}
