from inspect_ai import Task, task
from inspect_ai.dataset import FieldSpec, example_dataset
from inspect_ai.scorer import match

from evals.adapters.inspect_gsea import gsea_solver


@task
def local_biology() -> Task:
    return Task(
        dataset=example_dataset(
            "biology_qa",
            FieldSpec(input="question", target="answer"),
        ),
        solver=[gsea_solver()],
        scorer=match(location="any"),
    )
