import { useState } from 'react';
import { greeting } from './greeting.js';

export default function App() {
  const [count, setCount] = useState(0);
  return (
    <main style={{ fontFamily: 'system-ui, sans-serif', margin: '3rem auto', maxWidth: '40rem', padding: '0 1rem' }}>
      <h1>[[.Title]]</h1>
      <p>{greeting('[[.Title]]')}</p>
      <button type="button" onClick={() => setCount((c) => c + 1)}>
        Clicked {count} times
      </button>
    </main>
  );
}
