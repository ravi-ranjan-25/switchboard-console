export const dynamic = 'force-dynamic';

export default function Home() {
  return (
    <main>
      <h1>[[.Title]]</h1>
      <p>[[.Desc]]</p>
      <p>
        Health check: <a href="[[.Health]]">[[.Health]]</a>
      </p>
    </main>
  );
}
